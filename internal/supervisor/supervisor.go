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
	"slices"
	"sort"
	"strings"
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

	// cancel stops just this stream, and done closes once its goroutine has
	// fully returned. Both exist so one stream can be replaced without
	// disturbing the rest of the fleet.
	cancel context.CancelFunc
	done   chan struct{}
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

	// sinkFor is kept from AttachSinks, not merely applied once: Reload
	// builds sinks for cameras added after Run, and this is its only way to
	// reach the output that wants them.
	sinkFor func(camera, stream string) stream.FrameSink

	// runCtx is Run's context, the parent of every per-entry context, so a
	// stream started after Run still stops when the process shuts down.
	runCtx context.Context

	mu      sync.Mutex
	entries map[string]*entry

	// reloadMu serialises whole Reload calls, separately from mu. mu guards
	// the maps for short critical sections, including HTTP status reads, and
	// must never be held across the blocking stop phase; reloadMu is held
	// across exactly that phase, because two interleaved Reload calls would
	// otherwise each compute their diff against a different view of the
	// fleet and leave it matching neither config, with neither caller told
	// anything went wrong. Two browser tabs saving the camera list at once
	// is an ordinary occurrence once Reload is wired to an HTTP handler, not
	// an exotic one.
	//
	// Lock order: reloadMu is always acquired before mu, never the reverse.
	// Taking mu for the whole call instead of a dedicated lock would
	// deadlock the status endpoint behind a camera round trip.
	reloadMu sync.Mutex
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
		entries:      make(map[string]*entry),
	}
	for _, cam := range cams {
		for _, st := range cam.Streams {
			// One hub per camera+stream name so subscribers pick a specific
			// feed; "driveway/main" and "driveway/sub" are unrelated feeds
			// even though they share a camera.
			h := hub.New(64)
			name := hubName(cam.Name, st)
			s.hubs[name] = h
			s.entries[name] = &entry{
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
			}
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
	s.mu.Lock()
	// Kept, not just applied: Reload builds sinks for cameras added after
	// Run, and it has no other way to reach the output that wants them.
	s.sinkFor = sinkFor
	for _, e := range s.entries {
		s.applySinkLocked(e)
	}
	s.mu.Unlock()
}

// applySinkLocked gives e a frame sink if its camera asked for this stream
// over RTSP. Caller holds s.mu.
func (s *Supervisor) applySinkLocked(e *entry) {
	if s.sinkFor == nil {
		return
	}
	for _, want := range e.rtsp {
		if want == e.cfg.Stream {
			e.cfg.Sink = s.sinkFor(e.cfg.Name, e.cfg.Stream)
			return
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

// Hub returns one stream's hub by its "<camera>/<stream>" key. Locked
// because Reload adds and removes entries while the HTTP server is serving.
func (s *Supervisor) Hub(name string) (*hub.Hub, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hubs[name]
	return h, ok
}

// HubNames lists every stream currently configured.
func (s *Supervisor) HubNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.hubs))
	for name := range s.hubs {
		out = append(out, name)
	}
	return out
}

// ReloadResult reports what a Reload did, for the operator page to show and
// for a test to assert on.
type ReloadResult struct {
	Added     int
	Removed   int
	Restarted int
	Unchanged int
}

// ErrNotRunning is returned by Reload before Run has started. There is no
// context to start a stream under yet, and silently deferring the change
// until Run would make a reload that appeared to succeed do nothing.
var ErrNotRunning = errors.New("supervisor: not running")

// Validate reports whether cams would be accepted by Reload, without
// applying anything or requiring Run to have started.
//
// This exists so a caller writing a config file to disk, such as the
// control page's config editor, can check it against the running fleet's
// own constraints BEFORE the write, not just against config.Config's
// context-free rules. The two disagree in one real case: an [rtsp] section
// added to a daemon that booted without one passes config.Load (the file
// is self-consistent) but fails this exact check, because RTSP was never
// wired up on this Supervisor and cfg.RTSP stays nil here regardless of
// what the file says. Catching that before the file is written, rather
// than after, is what stops "saved, but not applied" from leaving a config
// on disk that the daemon's next restart cannot even boot from.
func (s *Supervisor) Validate(cams []config.Camera) error {
	cfg := config.Config{Cameras: cams}
	s.mu.Lock()
	hasRTSP := s.sinkFor != nil
	s.mu.Unlock()
	if hasRTSP {
		// Validate treats rtsp entries as an error without an [rtsp]
		// section, and by this point the RTSP server exists.
		cfg.RTSP = &config.RTSPConfig{}
	}
	return cfg.Validate()
}

// Reload brings the running fleet in line with cams, touching only what
// changed. A stream whose camera is unchanged keeps its connection, its hub
// and its subscribers: adding a ninth camera must not interrupt the other
// eight, which is the entire reason this exists rather than a process
// restart.
//
// A stream that is being replaced is stopped to completion before its
// replacement starts. Overlapping them gets the replacement a session the
// camera refuses, because the old one has not been released yet.
func (s *Supervisor) Reload(cams []config.Camera) (ReloadResult, error) {
	if !s.started.Load() {
		return ReloadResult{}, ErrNotRunning
	}

	// Held for the whole call, including across the stop phase below where
	// mu is deliberately released: see reloadMu's doc comment.
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	// Validate before touching anything. A reload that half applies leaves
	// a fleet in a state no config file describes.
	if err := s.Validate(cams); err != nil {
		return ReloadResult{}, err
	}

	want := make(map[string]config.Camera)
	for _, cam := range cams {
		for _, st := range cam.Streams {
			want[hubName(cam.Name, st)] = cam
		}
	}

	s.mu.Lock()

	var res ReloadResult
	var stop []*entry

	for key, e := range s.entries {
		cam, keep := want[key]
		if !keep {
			stop = append(stop, e)
			delete(s.entries, key)
			delete(s.hubs, key)
			res.Removed++
			continue
		}
		if sameStream(e, cam) {
			res.Unchanged++
			continue
		}
		stop = append(stop, e)
		delete(s.entries, key)
		delete(s.hubs, key)
		res.Restarted++
	}

	// Replacing names the keys that are being restarted rather than newly
	// added, so the two counts below do not both claim the same stream.
	replacing := make(map[string]bool, len(stop))
	for _, e := range stop {
		if _, stillWanted := want[hubName(e.cfg.Name, e.cfg.Stream)]; stillWanted {
			replacing[hubName(e.cfg.Name, e.cfg.Stream)] = true
		}
	}

	add := make([]string, 0, len(want))
	for key := range want {
		if _, exists := s.entries[key]; exists {
			continue
		}
		add = append(add, key)
	}
	s.mu.Unlock()

	// Stop outside the lock: stopEntry blocks until the stream-stop message
	// has gone out, and holding the lock through that would stall every
	// status request and every other reload for as long as a camera takes
	// to answer.
	for _, e := range stop {
		s.stopEntry(e)
	}

	s.mu.Lock()
	for _, key := range add {
		cam := want[key]
		st := strings.TrimPrefix(key, cam.Name+"/")
		h := hub.New(64)
		e := &entry{
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
		}
		s.applySinkLocked(e)
		s.entries[key] = e
		s.hubs[key] = h
		s.startLocked(e)
		if !replacing[key] {
			res.Added++
		}
	}
	s.mu.Unlock()

	return res, nil
}

// sameStream reports whether a running entry already matches cam, meaning
// nothing about its connection would change. The stream name is part of the
// key, so it is not compared here.
//
// The RTSP comparison is membership for this stream only, not the whole
// list: a camera gaining an RTSP entry for its sub stream is no reason to
// drop and rebuild its main stream's camera connection.
func sameStream(e *entry, cam config.Camera) bool {
	return e.cfg.Address == cam.Address &&
		e.cfg.Username == cam.Username &&
		e.cfg.Password == cam.Password &&
		slices.Contains(cam.RTSP, e.cfg.Stream) == slices.Contains(e.rtsp, e.cfg.Stream)
}

// Run starts one goroutine per configured stream and blocks until ctx is
// cancelled and every one of them has finished closing.
//
// Run may only be called once. A second concurrent call would spawn a
// second goroutine per entry, which is a second connection to every stream
// this Supervisor owns, the exact fault this package exists to prevent, so
// it returns ErrAlreadyRunning instead.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}

	s.mu.Lock()
	s.runCtx = ctx
	for _, e := range s.entries {
		s.startLocked(e)
	}
	s.mu.Unlock()

	<-ctx.Done()

	// Wait for every stream to finish, including any added by Reload after
	// Run started. Run must not return before every stream-stop message has
	// gone out: that is what releases each camera's session, and an exit
	// that races it leaves the whole fleet refusing connections for minutes.
	s.mu.Lock()
	live := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		live = append(live, e)
	}
	s.mu.Unlock()
	for _, e := range live {
		<-e.done
	}
	return ctx.Err()
}

// startLocked spawns the goroutine that owns e for its life. Caller holds
// s.mu.
func (s *Supervisor) startLocked(e *entry) {
	ctx, cancel := context.WithCancel(s.runCtx)
	e.cancel = cancel
	e.done = make(chan struct{})
	go func() {
		defer close(e.done)
		s.runStream(ctx, e)
	}()
}

// stopEntry cancels one stream and blocks until its goroutine has returned,
// which is after its stream-stop message has gone out. The wait is the
// point: restarting a camera before its previous session is released gets a
// connection that delivers nothing and blocks its own replacement until the
// camera times the dead session out.
func (s *Supervisor) stopEntry(e *entry) {
	if e.cancel == nil {
		return
	}
	e.cancel()
	<-e.done
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
	out := make([]StreamStat, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.stat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Camera != out[j].Camera {
			return out[i].Camera < out[j].Camera
		}
		return out[i].Stream < out[j].Stream
	})
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
