package supervisor

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

// runCounter records how many times each stream has been started, which is
// what "untouched" has to mean: not merely still running, but never
// restarted.
type runCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *runCounter) runner() Runner {
	return func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		c.mu.Lock()
		c.calls[cfg.Name+"/"+cfg.Stream]++
		c.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
}

func (c *runCounter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

func TestReloadLeavesUnchangedStreamsAlone(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	cams := []config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
		{Name: "b", Address: "2.2.2.2", Streams: []string{"main"}},
	}
	s := New(cams, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 && c.count("b/main") == 1 })

	res, err := s.Reload(append(cams, config.Camera{
		Name: "c", Address: "3.3.3.3", Streams: []string{"main"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Removed != 0 || res.Restarted != 0 || res.Unchanged != 2 {
		t.Fatalf("got %+v, want 1 added, 0 removed, 0 restarted, 2 unchanged", res)
	}
	waitFor(t, func() bool { return c.count("c/main") == 1 })
	if got := c.count("a/main"); got != 1 {
		t.Fatalf("a/main was started %d times, want 1: adding a camera disturbed an unrelated one", got)
	}
	if got := c.count("b/main"); got != 1 {
		t.Fatalf("b/main was started %d times, want 1", got)
	}
}

func TestReloadRemovesAStreamAndItsHub(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	cams := []config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main", "sub"}},
	}
	s := New(cams, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/sub") == 1 })

	res, err := s.Reload([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("got %+v, want 1 removed", res)
	}
	if _, ok := s.Hub("a/sub"); ok {
		t.Fatal("a/sub still has a hub after being removed")
	}
	if _, ok := s.Hub("a/main"); !ok {
		t.Fatal("a/main lost its hub")
	}
}

func TestReloadRestartsAStreamWhoseAddressChanged(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 })

	res, err := s.Reload([]config.Camera{
		{Name: "a", Address: "9.9.9.9", Streams: []string{"main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Restarted != 1 {
		t.Fatalf("got %+v, want 1 restarted", res)
	}
	if res.Added != 0 {
		t.Fatalf("got %+v: a replaced stream was counted as an addition too", res)
	}
	waitFor(t, func() bool { return c.count("a/main") == 2 })
}

func TestReloadRejectsAnInvalidCameraListWithoutTouchingAnything(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 })

	if _, err := s.Reload([]config.Camera{
		{Name: "a", Address: "", Streams: []string{"main"}},
	}); err == nil {
		t.Fatal("Reload accepted a camera with no address")
	}
	if _, ok := s.Hub("a/main"); !ok {
		t.Fatal("a rejected reload tore down the running fleet")
	}
	if got := c.count("a/main"); got != 1 {
		t.Fatalf("a/main restarted %d times on a rejected reload", got)
	}
}

// TestValidateCatchesWhatConfigLoadCannot is Finding 4 from the 2026-09-10
// review: a camera's rtsp entries pass config.Config.Validate whenever the
// candidate config's own [rtsp] section is present, but that says nothing
// about whether THIS Supervisor's RTSP server actually exists. A daemon
// that booted with no [rtsp] section has a nil sinkFor for its whole life;
// Validate must catch that mismatch so a caller can refuse to persist the
// change before it ever reaches disk, not find out only from Reload after
// the file is already written.
func TestValidateCatchesWhatConfigLoadCannotAboutRTSPWiring(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 })

	// No AttachSinks call: this Supervisor has no RTSP server, matching a
	// daemon that booted with no [rtsp] section.
	cams := []config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}, RTSP: []string{"main"}},
	}
	if err := s.Validate(cams); err == nil {
		t.Fatal("Validate accepted an rtsp entry on a Supervisor with no RTSP wiring")
	}
	if _, err := s.Reload(cams); err == nil {
		t.Fatal("Reload accepted the same camera list Validate should have rejected")
	}
}

// TestReloadRefusedOnceShuttingDown is Finding 5 from the 2026-09-10
// review: a Reload racing shutdown can leave a stream-stop unwaited when
// the process exits right after Run returns. Refusing any Reload that
// starts once ctx is already done closes the window for a Reload that has
// not yet begun; TestReloadRacingShutdownIsFullyAwaited below covers the
// other half, a Reload already in flight when ctx is cancelled.
func TestReloadRefusedOnceShuttingDown(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitFor(t, func() bool { return c.count("a/main") == 1 })

	cancel()
	<-runDone

	_, err := s.Reload([]config.Camera{
		{Name: "b", Address: "2.2.2.2", Streams: []string{"main"}},
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("got err %v, want ErrShuttingDown", err)
	}
}

// TestReloadRacingShutdownIsFullyAwaited reproduces the 2026-09-08 failure
// mode directly: a Reload already past Reload's own shutdown check, mid
// stop-phase, when ctx is cancelled. Run must not return until that stop
// phase (and therefore that stream's stream-stop message) has actually
// completed, not merely until the entry disappears from the map.
func TestReloadRacingShutdownIsFullyAwaited(t *testing.T) {
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	var stopped atomic.Bool

	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		if cfg.Name == "a" {
			// Simulate a slow stream-stop round trip: this is the work
			// that must finish before the process is allowed to exit.
			close(stopStarted)
			<-releaseStop
			stopped.Store(true)
		}
		return ctx.Err()
	}

	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	waitFor(t, func() bool {
		for _, st := range s.Stats() {
			if st.Camera == "a" && st.Running {
				return true
			}
		}
		return false
	})

	reloadDone := make(chan error, 1)
	go func() {
		// Removing "a" drives it through Reload's stop phase, which is
		// where stopEntry blocks on run's ctx.Done() branch above.
		_, err := s.Reload(nil)
		reloadDone <- err
	}()

	select {
	case <-stopStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Reload's stop phase never reached the runner")
	}

	// Cancel while Reload is blocked mid-stop: this is the exact race.
	cancel()

	select {
	case <-runDone:
		t.Fatal("Run returned before the in-flight Reload's stop phase finished")
	case <-time.After(100 * time.Millisecond):
	}

	if stopped.Load() {
		t.Fatal("the runner's stop path completed before Run was even given the chance to wait for it, test is not exercising the race")
	}
	close(releaseStop)

	if err := <-reloadDone; err != nil {
		t.Fatalf("Reload: %v", err)
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the in-flight Reload's stop phase finished")
	}
	if !stopped.Load() {
		t.Fatal("Run returned without the stream-stop path having completed")
	}
}

func TestReloadBeforeRunIsRefused(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New(nil, c.runner())
	if _, err := s.Reload(nil); err == nil {
		t.Fatal("Reload before Run was accepted; there is no context to start streams under")
	}
}

// TestReloadCannotObserveStartedWithoutRunCtx drives, deterministically and
// without any goroutine timing, the exact window that used to let Reload
// reach startLocked with a nil runCtx: started flipping true before runCtx
// is assigned. Run sets both together under s.mu now, so this state is no
// longer reachable through Run itself; this test forces it directly to
// prove Reload's own gate, not just Run's ordering, refuses to proceed
// without a runCtx. Before the fix, Reload trusted started.Load() alone,
// which is true here, and panicked inside startLocked's
// context.WithCancel(nil).
func TestReloadCannotObserveStartedWithoutRunCtx(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())

	// Simulates the instant after Run's CAS succeeds but before it has
	// assigned runCtx: started is true, runCtx is still nil.
	s.started.Store(true)

	_, err := s.Reload([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
		{Name: "b", Address: "2.2.2.2", Streams: []string{"main"}},
	})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Reload with started=true and runCtx=nil returned %v, want ErrNotRunning", err)
	}
}

// TestReloadSerialisesConcurrentCalls fires two Reload calls at once with
// different target camera lists, the way two browser tabs saving the camera
// list at the same time would. Without reloadMu, both calls read the fleet
// before either has written its changes back, compute their diffs against
// that same stale view, and interleave their writes: the result can end up
// with pieces of both configs, or with a stream neither config names. This
// asserts the final fleet is exactly one config or exactly the other, never
// a mixture, and that every stream in it is actually running.
func TestReloadSerialisesConcurrentCalls(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New([]config.Camera{
		{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}},
	}, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 })

	configX := []config.Camera{
		{Name: "a", Address: "9.9.9.9", Streams: []string{"main"}},
		{Name: "b", Address: "2.2.2.2", Streams: []string{"main"}},
	}
	configY := []config.Camera{
		{Name: "a", Address: "8.8.8.8", Streams: []string{"main"}},
		{Name: "c", Address: "3.3.3.3", Streams: []string{"main"}},
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = s.Reload(configX)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = s.Reload(configY)
	}()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	names := s.HubNames()
	sort.Strings(names)
	matchesX := len(names) == 2 && names[0] == "a/main" && names[1] == "b/main"
	matchesY := len(names) == 2 && names[0] == "a/main" && names[1] == "c/main"
	if !matchesX && !matchesY {
		t.Fatalf("fleet after concurrent reloads is %v, want exactly configX's streams or exactly configY's, not a mixture", names)
	}

	var wantAddr string
	if matchesX {
		wantAddr = "9.9.9.9"
	} else {
		wantAddr = "8.8.8.8"
	}
	s.mu.Lock()
	gotAddr := s.entries["a/main"].cfg.Address
	s.mu.Unlock()
	if gotAddr != wantAddr {
		t.Fatalf("a/main has address %q, want %q: the fleet matches one config's names but another's data", gotAddr, wantAddr)
	}

	for _, name := range names {
		waitFor(t, func() bool {
			for _, stat := range s.Stats() {
				if hubName(stat.Camera, stat.Stream) == name {
					return stat.Running
				}
			}
			return false
		})
	}
}
