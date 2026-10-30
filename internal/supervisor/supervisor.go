// Package supervisor keeps every configured camera stream running, one
// goroutine per stream, restarting it with backoff when it fails.
//
// A camera's main, sub and extern streams are independent Baichuan
// connections, so they run on separate goroutines rather than being
// serialized behind one lock per camera; a stall on one must never delay
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

	"github.com/VoltTech21/reostream/internal/baichuan"
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

// Runner runs one camera stream until ctx is canceled or it fails. It is a
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
//
// LastError is history, not a diagnosis: it is the last error this stream
// ever hit and is never cleared, because a stream that failed and recovered
// is exactly what someone reading a status page needs to know about. That
// makes it dangerous on its own, which is what LastErrorAt is for. On
// 2026-10-16 a fisheye substream carried "camera is answering but not
// sending media (held session)" from a single restart hours earlier while
// streaming at 9.8 fps with a frame 0.1s old, and the stale sentence was
// read as a live fault and reported as a dead feed.
type StreamStat struct {
	Camera      string
	Stream      string
	Running     bool
	LastError   string
	LastErrorAt time.Time
	Restarts    int
	// OnDemand marks a battery camera's stream, which is asleep rather
	// than down when it is not running.
	OnDemand bool
	// Privacy marks a stream whose camera is in privacy mode
	// (stream.ErrPrivacy): off on purpose, not failing.
	Privacy bool
	// started is when the current run began, so Privacy can clear once a
	// run has lasted long enough to be streaming.
	started time.Time
}

// privacyRetry is how often a camera in privacy mode is checked again. It
// is steady, not the error backoff: privacy mode is a choice, and the
// stream should come back soon after it is turned off without the camera
// being asked every second while it is on.
var privacyRetry = 15 * time.Second

// entry is everything one goroutine in runStream needs for the one stream
// it owns for its entire life.
type entry struct {
	cfg stream.Config
	h   *hub.Hub
	// rtsp is the camera's rtsp stream list, kept so AttachSinks can tell
	// which of a camera's streams an output was asked for.
	rtsp []string
	stat StreamStat

	// onDemand is a battery camera's stream: run only while someone is
	// watching, never held open. See runOnDemand.
	onDemand bool
	// alwaysOn keeps an on-demand stream awake around the clock: a battery
	// camera on constant power (config always_on).
	alwaysOn bool
	// privacyKick ends a privacy-mode wait early (PrivacyOff).
	privacyKick chan struct{}
	// motionUntil (Unix nanoseconds) keeps an on-demand stream awake with
	// nobody watching while the camera reports motion. See Motion.
	motionUntil atomic.Int64

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

	// presenceFor gives a battery camera's on-demand main stream its sink:
	// the presence stream, which a recorder reads without waking the
	// camera. See AttachPresence.
	presenceFor func(camera string, h *hub.Hub) stream.FrameSink

	// stillDir keeps battery cameras' last pictures across restarts.
	stillDir string

	// motionHook hears motion from battery cameras' wakes. See
	// SetMotionHook.
	motionHook func(camera string, on bool)

	// runCtx is Run's context, the parent of every per-entry context, so a
	// stream started after Run still stops when the process shuts down.
	runCtx context.Context

	mu      sync.Mutex
	entries map[string]*entry

	// reloadMu serializes whole Reload calls, separately from mu. mu guards
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

	// failures is each failing camera's state, so its log says so once
	// and then stays quiet; see failures.go.
	failMu   sync.Mutex
	failures map[string]*camFailure
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
		streams, _ := cameraStreams(cam)
		for _, st := range streams {
			// One hub per camera+stream name so subscribers pick a specific
			// feed; "driveway/main" and "driveway/sub" are unrelated feeds
			// even though they share a camera.
			e := newEntry(cam, st)
			s.hubs[hubName(cam.Name, st)] = e.h
			s.entries[hubName(cam.Name, st)] = e
		}
	}
	return s
}

// cameraStreams is the streams a camera runs. A battery camera that lists
// none gets sub and main on demand: they exist so a viewer can wake it,
// and run only while one is watching.
func cameraStreams(cam config.Camera) (streams []string, onDemand bool) {
	if len(cam.Streams) == 0 && cam.UID != "" {
		return []string{"sub", "main"}, true
	}
	return cam.Streams, false
}

func newEntry(cam config.Camera, st string) *entry {
	_, onDemand := cameraStreams(cam)
	h := hub.New(64)
	h.SetOnDemand(onDemand)
	return &entry{
		privacyKick: make(chan struct{}, 1),
		rtsp:        cam.RTSP,
		cfg: stream.Config{
			Name:     cam.Name,
			Address:  cam.Address,
			Username: cam.Username,
			Password: cam.Password,
			Stream:   st,
			Channel:  cam.Channel,
			UID:      cam.UID,

			HoldPicture: cam.HoldPicture,
			Rotate:      cam.Rotate,
		},
		h:        h,
		stat:     StreamStat{Camera: cam.Name, Stream: st, OnDemand: onDemand},
		onDemand: onDemand,
		alwaysOn: onDemand && cam.AlwaysOn,
	}
}

// CameraConfig returns the connection details the supervisor currently
// holds for camera name, reflecting any Reload since startup.
func (s *Supervisor) CameraConfig(name string) (stream.Config, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.cfg.Name == name {
			return e.cfg, true
		}
	}
	return stream.Config{}, false
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

// AttachPresence gives each battery camera's main stream the sink built by
// presenceFor. Call before Run, like AttachSinks.
func (s *Supervisor) AttachPresence(presenceFor func(camera string, h *hub.Hub) stream.FrameSink) {
	if s.started.Load() {
		panic("supervisor: AttachPresence called after Run")
	}
	s.mu.Lock()
	s.presenceFor = presenceFor
	for _, e := range s.entries {
		s.applySinkLocked(e)
	}
	s.mu.Unlock()
}

// applySinkLocked gives e a frame sink if its camera asked for this stream
// over RTSP. Caller holds s.mu.
func (s *Supervisor) applySinkLocked(e *entry) {
	// A battery camera's on-demand stream never feeds RTSP directly: a
	// recorder pulling it would hold the camera awake around the clock.
	// Its main stream feeds the presence stream instead, which a recorder
	// can hold open because it never reaches the camera.
	if e.onDemand {
		if s.presenceFor != nil && e.cfg.Stream == "main" {
			// The hub goes in so the sink can read the camera's kept still
			// without calling back into the supervisor, whose lock is
			// held here.
			e.cfg.Sink = s.presenceFor(e.cfg.Name, e.h)
		}
		return
	}
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
// read without synchronization for the whole life of each stream goroutine,
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

// ErrShuttingDown is returned by Reload once Run's context has been
// canceled. Run's final wait (see Run) only waits for entries still in the
// map at the moment it checks; a Reload allowed to start after ctx is
// already done could still be mid-diff or mid-stop when Run returns and the
// process exits out from under it, which is exactly what leaves a
// stream-stop message unwaited. Refusing outright, rather than racing it to
// completion, closes that window for any Reload that has not already
// begun; see Run's own reloadMu wait for the one that has.
var ErrShuttingDown = errors.New("supervisor: shutting down")

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
	// Likewise motion_topic needs an [events] broker, which this camera
	// list does not carry: the whole file's own validation has checked
	// that already. Without this every save from the page failed on a
	// config that uses motion_topic.
	for _, c := range cams {
		if c.MotionTopic != "" {
			cfg.Events = &config.EventsConfig{MQTT: "checked with the file"}
			break
		}
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
		streams, _ := cameraStreams(cam)
		for _, st := range streams {
			want[hubName(cam.Name, st)] = cam
		}
	}

	s.mu.Lock()

	// The authoritative "is this Supervisor running" check, not the
	// started.Load() above. That check is only a cheap early-out for the
	// common case of Reload arriving before Run at all; started flips true
	// inside Run before runCtx is assigned, so a Reload that read it
	// concurrently with Run's own startup could otherwise pass it and reach
	// startLocked below with s.runCtx still nil, and context.WithCancel(nil)
	// panics. Rechecking runCtx itself, under the same mutex Run assigns it
	// under, is what actually closes that window: Run's mu-protected section
	// now sets started and runCtx together (see Run), so by the time this
	// goroutine holds mu, either runCtx is already set or Run has not
	// started at all.
	if s.runCtx == nil {
		s.mu.Unlock()
		return ReloadResult{}, ErrNotRunning
	}
	if s.runCtx.Err() != nil {
		s.mu.Unlock()
		return ReloadResult{}, ErrShuttingDown
	}

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
		e := newEntry(cam, st)
		if e.onDemand {
			loadStill(s.stillDir, e)
		}
		s.applySinkLocked(e)
		s.entries[key] = e
		s.hubs[key] = e.h
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
	_, onDemand := cameraStreams(cam)
	return e.onDemand == onDemand &&
		e.alwaysOn == (onDemand && cam.AlwaysOn) &&
		e.cfg.Address == cam.Address &&
		e.cfg.Channel == cam.Channel &&
		e.cfg.UID == cam.UID &&
		e.cfg.Username == cam.Username &&
		e.cfg.Password == cam.Password &&
		slices.Contains(cam.RTSP, e.cfg.Stream) == slices.Contains(e.rtsp, e.cfg.Stream)
}

// Run starts one goroutine per configured stream and blocks until ctx is
// canceled and every one of them has finished closing.
//
// Run may only be called once. A second concurrent call would spawn a
// second goroutine per entry, which is a second connection to every stream
// this Supervisor owns, the exact fault this package exists to prevent, so
// it returns ErrAlreadyRunning instead.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if !s.started.CompareAndSwap(false, true) {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	// runCtx is assigned inside the same critical section as the CAS above,
	// not after it. Reload's authoritative gate is runCtx being non-nil,
	// read under this same mutex (see Reload); if the CAS and this
	// assignment were separate critical sections, a Reload landing between
	// them could observe started already true while runCtx was still nil.
	// Locking them together means any goroutine that acquires mu after
	// observing started==true is guaranteed to see runCtx already set,
	// because this Unlock happens-after both.
	s.runCtx = ctx
	for _, e := range s.entries {
		s.startLocked(e)
	}
	s.mu.Unlock()

	<-ctx.Done()

	// Wait for a Reload call already in flight to finish its own stop
	// phase before deciding what is still live. Reload holds reloadMu for
	// its entire call, including that stop phase (see Reload's own
	// comment on why the stop happens outside mu), and it removes an
	// entry from s.entries before stopping it; without this, a Reload that
	// began just before ctx was canceled can have already removed an
	// entry this snapshot would otherwise miss, and Run would return while
	// that entry's stream-stop message is still in flight on another
	// goroutine the process is about to kill. A Reload that has not yet
	// acquired reloadMu at this point instead finds runCtx already done
	// once it does (see the check at the top of Reload) and refuses
	// outright, so it never reaches the stop phase at all.
	s.reloadMu.Lock()
	s.reloadMu.Unlock()

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
// runs again, until ctx is canceled. There is exactly one call to s.run in
// flight for this stream at any time, which is the invariant the whole
// supervisor exists to hold: a camera allows only one connection per
// stream, and a second one gets a session that delivers nothing and blocks
// its own replacement until the camera times the dead one out.
func (s *Supervisor) runStream(ctx context.Context, e *entry) {
	if e.onDemand {
		s.runOnDemand(ctx, e)
		return
	}
	backoff := s.backoffStart
	for {
		if ctx.Err() != nil {
			// Covers a context that was already canceled before this
			// goroutine got scheduled, not just the check further down: an
			// already-canceled ctx handed to Run must not leak this hub's
			// subscribers either.
			e.h.Close()
			return
		}

		s.setRunning(e, true)
		start := time.Now()
		s.mu.Lock()
		e.stat.started = start
		s.mu.Unlock()
		// A failing camera coming back is noticed as soon as this run
		// delivers, not when it next ends.
		running := make(chan struct{})
		go func(cam string) {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-running:
					return
				case <-t.C:
					if !e.h.Stats().ConnectedAt.IsZero() {
						s.noteRecovered(cam)
						return
					}
				}
			}
		}(e.cfg.Name)
		err := s.run(ctx, e.cfg, e.h)
		close(running)
		ran := time.Since(start)
		s.setResult(e, err)

		if errors.Is(err, stream.ErrPrivacy) && ctx.Err() == nil {
			s.mu.Lock()
			was := e.stat.Privacy
			e.stat.Privacy = true
			s.mu.Unlock()
			if !was {
				log.Printf("reostream: %s: the camera is in privacy mode; checking again every %s", hubName(e.cfg.Name, e.cfg.Stream), privacyRetry)
			}
			backoff = s.backoffStart
			select {
			case <-ctx.Done():
				e.h.Close()
				return
			case <-time.After(privacyRetry):
			case <-e.kick():
			}
			continue
		}
		s.mu.Lock()
		if e.stat.Privacy {
			e.stat.Privacy = false
			log.Printf("reostream: %s: privacy mode is off", hubName(e.cfg.Name, e.cfg.Stream))
		}
		s.mu.Unlock()

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
		if err != nil && ctx.Err() == nil {
			s.noteFailure(e.cfg.Name, err)
		}

		select {
		case <-ctx.Done():
			e.h.Close()
			return
		case <-time.After(retryAfter(err, backoff)):
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
		// A run that has lasted half a privacy check is streaming: the
		// camera left privacy mode.
		if e.stat.Privacy && e.stat.Running && time.Since(e.stat.started) >= privacyRetry/2 {
			e.stat.Privacy = false
			log.Printf("reostream: %s: privacy mode is off", hubName(e.cfg.Name, e.cfg.Stream))
		}
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
		e.stat.LastError = plainError(err)
		e.stat.LastErrorAt = time.Now()
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

// kick is the entry's privacy-wait channel.
func (e *entry) kick() chan struct{} { return e.privacyKick }

// PrivacyOff tells camera's streams that privacy mode was just turned off,
// so they try again now rather than at the next check.
func (s *Supervisor) PrivacyOff(camera string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.cfg.Name == camera && e.stat.Privacy {
			select {
			case e.kick() <- struct{}{}:
			default:
			}
		}
	}
}

// authRetry is how long a stream waits after a wrong password before trying
// again. Cameras and NVRs count failed logins and lock them out, an NVR after
// ten (RLN8-410, emulated), so retrying at the usual pace would lock out the
// right password too once it is fixed, and a password does not fix itself
// in fifteen seconds.
const authRetry = 5 * time.Minute

func retryAfter(err error, backoff time.Duration) time.Duration {
	if errors.Is(err, baichuan.ErrUnauthorized) {
		return authRetry
	}
	return backoff
}
