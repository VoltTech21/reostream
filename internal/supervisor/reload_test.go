package supervisor

import (
	"context"
	"sort"
	"sync"
	"testing"

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

func TestReloadBeforeRunIsRefused(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	s := New(nil, c.runner())
	if _, err := s.Reload(nil); err == nil {
		t.Fatal("Reload before Run was accepted; there is no context to start streams under")
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
