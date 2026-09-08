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
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

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
	cfg  stream.Config
	h    *hub.Hub
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

// setBackoff overrides the backoff parameters. Unexported: production code
// always runs on the defaults, and only this package's own tests, which
// share the package, can reach into a Supervisor to shorten them.
func (s *Supervisor) setBackoff(start, max, reset time.Duration) {
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
func (s *Supervisor) Run(ctx context.Context) error {
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

		select {
		case <-ctx.Done():
			e.h.Close()
			return
		case <-time.After(backoff):
		}

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
	e.stat.Restarts++
	s.mu.Unlock()
}
