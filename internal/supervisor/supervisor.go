// Package supervisor keeps every configured camera stream running, one
// goroutine per stream, restarting it with backoff when it fails.
//
// A camera's main, sub and extern streams are independent Baichuan
// connections, so they run on separate goroutines rather than being
// serialised behind one lock per camera; a stall on one must never delay
// the others.
package supervisor

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

// ErrAlreadyRunning is returned by a second call to Run on the same
// Supervisor. Two concurrent Run calls would spawn two goroutines per
// entry, which is exactly the two-connections-to-one-stream overlap this
// package exists to prevent, so a second call is refused rather than
// silently duplicating the fleet.
var ErrAlreadyRunning = errors.New("supervisor: already running")

// Runner runs one camera stream until ctx is cancelled or it fails. It is a
// parameter rather than a hard call to stream.Run so the supervisor can be
// tested without a camera.
type Runner func(ctx context.Context, cfg stream.Config, h *hub.Hub) error

// defaultBackoffStart, defaultBackoffMax and defaultBackoffReset: a camera
// refusing connections must not become a tight reconnect loop that hammers
// it and fills the log, but a stream that has been healthy for a while
// should not carry a long backoff into its next, unrelated failure.
//
// defaultBackoffStart is 1s, not something faster, for a reason specific to
// these cameras: the most common cause of a refused or empty connection is
// the camera still holding a session from a client that died without
// sending stream-stop, and that clears in minutes, not milliseconds.
// Retrying eight times in the first second accomplishes nothing but load on
// a camera that is already unhappy, and buries the log entry that would
// have shown the actual cause. defaultBackoffMax is where a camera that is
// genuinely down settles, low enough to notice recovery within seconds of
// it coming back.
const (
	defaultBackoffStart = 1 * time.Second
	defaultBackoffMax   = 15 * time.Second
	defaultBackoffReset = 1 * time.Minute
)

// StreamStat reports one stream's current state for status endpoints.
type StreamStat struct {
	Camera    string
	Stream    string
	Running   bool
	LastError string
	Restarts  int
}

// entry is everything one goroutine in runStream needs for the one stream
// it owns for its entire life.
type entry struct {
	cfg stream.Config
	h   *hub.Hub
	// rtsp is the camera's rtsp stream list, kept so AttachSinks can tell
	// which of a camera's streams an output was asked for.
	rtsp []string
	stat StreamStat
}

// Supervisor runs every stream from a camera list and restarts failed ones.
type Supervisor struct {
	run Runner

	// backoffStart, backoffMax and backoffReset default to the package
	// constants above and are only ever changed by setBackoff, which exists
	// so tests can use a short, deterministic backoff instead of bending
	// the production value to fit a test's timeout. See
	// TestDefaultBackoffStartIsOneSecond for the regression guard on that.
	backoffStart time.Duration
	backoffMax   time.Duration
	backoffReset time.Duration

	// started guards Run against a second concurrent call and, as a side
	// effect, tells setBackoff when it is too late to change anything: once
	// runStream goroutines exist they read the backoff fields with no lock
	// of their own, so a change after start would be a real race rather
	// than the harmless one the race detector happens not to catch today.
	started atomic.Bool

	hubs map[string]*hub.Hub

	mu      sync.Mutex
	entries []*entry
}

// New builds a Supervisor for cams, using run to drive each stream. Nothing
// starts until Run is called.
func New(cams []config.Camera, run Runner) *Supervisor {
	s := &Supervisor{
		run:          run,
		backoffStart: defaultBackoffStart,
		backoffMax:   defaultBackoffMax,
		backoffReset: defaultBackoffReset,
		hubs:         make(map[string]*hub.Hub),
	}
	for _, cam := range cams {
		for _, st := range cam.Streams {
			// One hub per camera+stream name so subscribers pick a specific
			// feed; "driveway/main" and "driveway/sub" are unrelated feeds
			// even though they share a camera.
			h := hub.New(64)
			s.hubs[hubName(cam.Name, st)] = h
			s.entries = append(s.entries, &entry{
				rtsp: cam.RTSP,
				cfg: stream.Config{
					Name:     cam.Name,
					Address:  cam.Address,
					Username: cam.Username,
					Password: cam.Password,
					Stream:   st,
				},
				h:    h,
				stat: StreamStat{Camera: cam.Name, Stream: st},
			})
		}
	}
	return s
}

// AttachSinks gives every stream named in a camera's rtsp list a frame sink,
// built by sinkFor.
//
// It takes a factory rather than an RTSP server so this package does not
// depend on that one: the supervisor's job is running camera connections, and
// which outputs consume them is not its concern. A camera with no rtsp entry
// keeps a nil sink, which is the state the HTTP path has always run in.
//
// Call before Run. It panics afterwards rather than racing a live fleet,
// matching setBackoff.
func (s *Supervisor) AttachSinks(sinkFor func(camera, stream string) stream.FrameSink) {
	if s.started.Load() {
		panic("supervisor: AttachSinks called after Run")
	}
	for _, e := range s.entries {
		for _, want := range e.rtsp {
			if want != e.cfg.Stream {
				continue
			}
			e.cfg.Sink = sinkFor(e.cfg.Name, e.cfg.Stream)
			break
		}
	}
}

// setBackoff overrides the backoff parameters. Unexported: production code
// always runs on the defaults, and only this package's own tests, which
// share the package, can reach into a Supervisor to shorten them, and only
// before Run starts. It panics if called after start rather than taking a
// lock and letting a live Supervisor be retuned mid-run: the values are
// read without synchronisation for the whole life of each stream goroutine,
// snapshotting or locking around every read would cost every stream a lock
// operation per cycle forever to protect a knob that is only ever set once,
// in a test, before anything is running.
func (s *Supervisor) setBackoff(start, max, reset time.Duration) {
	if s.started.Load() {
		panic("supervisor: setBackoff called after Run has started")
	}
	s.backoffStart = start
	s.backoffMax = max
	s.backoffReset = reset
}

// hubName is the key streams are published under and looked up by:
// "<camera>/<stream>".
func hubName(camera, stream string) string {
	return camera + "/" + stream
}

// Hubs returns every stream's hub, keyed by "<camera>/<stream>". Safe to
// call at any time; the map itself is never mutated after New.
func (s *Supervisor) Hubs() map[string]*hub.Hub {
	return s.hubs
}

// Run starts one goroutine per configured stream and blocks until ctx is
// cancelled and every one of them has finished closing.
//
// Run must not return before every stream has stopped: sending the
// stream-stop message is what releases the camera's session, and an exit
// that races that close leaves the camera refusing connections on that
// stream for minutes. That is why this is a plain WaitGroup rather than
// returning as soon as ctx is done.
//
// Run may only be called once. A second concurrent call would spawn a
// second goroutine per entry, which is a second connection to every stream
// this Supervisor owns, the exact fault this package exists to prevent, so
// it returns ErrAlreadyRunning instead.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}

	var wg sync.WaitGroup
	for _, e := range s.entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			s.runStream(ctx, e)
		}(e)
	}
	wg.Wait()
	return ctx.Err()
}

// runStream is the one goroutine that owns a single camera stream for its
// entire life: it runs, waits for that call to fully return, backs off, and
// runs again, until ctx is cancelled. There is exactly one call to s.run in
// flight for this stream at any time, which is the invariant the whole
// supervisor exists to hold: a camera allows only one connection per
// stream, and a second one gets a session that delivers nothing and blocks
// its own replacement until the camera times the dead one out.
func (s *Supervisor) runStream(ctx context.Context, e *entry) {
	backoff := s.backoffStart
	for {
		if ctx.Err() != nil {
			// Covers a context that was already cancelled before this
			// goroutine got scheduled, not just the check further down: an
			// already-cancelled ctx handed to Run must not leak this hub's
			// subscribers either.
			e.h.Close()
			return
		}

		s.setRunning(e, true)
		start := time.Now()
		err := s.run(ctx, e.cfg, e.h)
		ran := time.Since(start)
		s.setResult(e, err)

		if ctx.Err() != nil {
			// The stream is going away for good: release anyone subscribed
			// to its hub rather than leaving them to wait out an HTTP
			// shutdown timeout.
			e.h.Close()
			return
		}

		if ran > s.backoffReset {
			backoff = s.backoffStart
		}

		// The status endpoint carries last_error, but a stream that has been
		// failing and reconnecting for an hour leaves nothing behind in the
		// log to say when it started or how often: during the fleet cutover
		// two streams sat at 19 restarts with an empty container log, which
		// told an operator nothing about what to fix.
		if err != nil {
			log.Printf("reostream: %s: %v; reconnecting in %s",
				hubName(e.cfg.Name, e.cfg.Stream), err, backoff)
		}

		select {
		case <-ctx.Done():
			e.h.Close()
			return
		case <-time.After(backoff):
		}

		// Only reached by actually looping back to run again, so this
		// counts restarts, not the initial run: a stream that has never
		// failed reports 0, not 1.
		s.incrementRestarts(e)

		backoff *= 2
		if backoff > s.backoffMax {
			backoff = s.backoffMax
		}
	}
}

// Stats returns a snapshot of every stream's current state.
func (s *Supervisor) Stats() []StreamStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StreamStat, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.stat
	}
	return out
}

func (s *Supervisor) setRunning(e *entry, running bool) {
	s.mu.Lock()
	e.stat.Running = running
	s.mu.Unlock()
}

func (s *Supervisor) setResult(e *entry, err error) {
	s.mu.Lock()
	e.stat.Running = false
	if err != nil {
		e.stat.LastError = err.Error()
	}
	s.mu.Unlock()
}

// incrementRestarts counts a stream actually restarting, called only from
// the path that is about to run it again, not from its first run.
func (s *Supervisor) incrementRestarts(e *entry) {
	s.mu.Lock()
	e.stat.Restarts++
	s.mu.Unlock()
}
