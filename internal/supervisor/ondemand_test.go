package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

// fastWake shortens the battery limits for a test and puts them back.
func fastWake(t *testing.T) {
	t.Helper()
	idle, max, cool, poll, retry := wakeIdle, wakeMax, wakeCooldown, wakePoll, wakeRetry
	sw := stillWait
	stillWait = map[string]time.Duration{"sub": 0, "main": 0}
	t.Cleanup(func() { stillWait = sw })
	wakeIdle, wakeMax, wakeCooldown, wakePoll, wakeRetry = 100*time.Millisecond, 400*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { wakeIdle, wakeMax, wakeCooldown, wakePoll, wakeRetry = idle, max, cool, poll, retry })
	// Keep the wake watcher off the network: no camera answers its search.
	search := wakeSearch
	wakeSearch = func(context.Context, string) (bcudp.Info, error) { return bcudp.Info{}, errors.New("no camera") }
	t.Cleanup(func() { wakeSearch = search })
}

var battery = config.Camera{Name: "porch", Address: "192.0.2.10", UID: "95270000TESTUID1"}

// counting runs a session until it is canceled, counting wakes and
// tracking whether one is in progress.
func counting(wakes, awake *int32) Runner {
	return func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(wakes, 1)
		atomic.StoreInt32(awake, 1)
		<-ctx.Done()
		atomic.StoreInt32(awake, 0)
		return nil
	}
}

func TestBatteryCameraSleepsWithNoViewer(t *testing.T) {
	fastWake(t)
	var wakes, awake int32
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if wakes != 0 {
		t.Fatalf("woke %d times with nobody watching", wakes)
	}
	h, ok := s.Hub("porch/sub")
	if !ok || !h.OnDemand() {
		t.Fatal("battery camera has no on-demand sub hub")
	}
}

func TestBatteryCameraWakesForAViewerAndSleepsAfter(t *testing.T) {
	fastWake(t)
	var wakes, awake int32
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	start(t, s)

	h, _ := s.Hub("porch/sub")
	_, unsub := h.Subscribe()
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 1 })
	unsub()
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 0 })
	if n := atomic.LoadInt32(&wakes); n != 1 {
		t.Fatalf("woke %d times for one viewer", n)
	}
}

// A viewer that never leaves still lets the camera sleep at wakeMax, and
// is disconnected so it does not wake it straight back up.
func TestBatteryCameraSleepsAtTheSessionLimit(t *testing.T) {
	fastWake(t)
	var wakes, awake int32
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	start(t, s)

	h, _ := s.Hub("porch/sub")
	ch, unsub := h.Subscribe()
	defer unsub()
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 1 })
	select {
	case _, open := <-ch:
		for open {
			_, open = <-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("viewer was never disconnected")
	}
	if atomic.LoadInt32(&awake) != 0 {
		t.Fatal("camera still awake after the session limit")
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&wakes); n != 1 {
		t.Fatalf("woke %d times; the session limit should end it", n)
	}
}

// A camera that will not wake is tried a few times, then the viewer is
// let go rather than retried forever.
func TestBatteryCameraGivesUpOnAFailedWake(t *testing.T) {
	fastWake(t)
	var tries int32
	s := New([]config.Camera{battery}, func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(&tries, 1)
		return errors.New("no answer")
	})
	start(t, s)

	h, _ := s.Hub("porch/sub")
	ch, unsub := h.Subscribe()
	defer unsub()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("got data from a camera that never woke")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("viewer kept waiting on a camera that will not wake")
	}
	if n := atomic.LoadInt32(&tries); n != int32(wakeTries) {
		t.Fatalf("tried %d times, want %d", n, wakeTries)
	}
}

func TestBatteryStreamsGetNoRTSP(t *testing.T) {
	cam := battery
	cam.RTSP = []string{"sub", "main"}
	s := New([]config.Camera{cam}, counting(new(int32), new(int32)))
	s.AttachSinks(func(camera, st string) stream.FrameSink {
		t.Fatalf("RTSP sink built for battery stream %s/%s", camera, st)
		return nil
	})
}

// start runs s until the test ends, and waits for it to stop before the
// test's cleanups, fastWake's included, run.
func start(t *testing.T, s *Supervisor) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// A battery camera's last picture outlives a restart: kept after a wake,
// put back when the next process starts.
func TestBatteryStillSurvivesARestart(t *testing.T) {
	fastWake(t)
	dir := t.TempDir()
	key := []byte{0, 0, 0, 1, 0x65, 0x88}
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		h.SetKeyframe("H264", key)
		<-ctx.Done()
		return nil
	}
	s := New([]config.Camera{battery}, run)
	s.SetStillDir(dir)
	start(t, s)
	h, _ := s.Hub("porch/sub")
	_, unsub := h.Subscribe()
	waitFor(t, func() bool { _, es := h.Keyframe(); return es != nil })
	unsub()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "porch_sub.still")); return err == nil })

	again := New([]config.Camera{battery}, run)
	again.SetStillDir(dir)
	h2, _ := again.Hub("porch/sub")
	codec, es := h2.Keyframe()
	if codec != "H264" || string(es) != string(key) || h2.KeyframeAt().IsZero() {
		t.Fatalf("restored %q %v", codec, es)
	}
}

// With nothing to show, a battery camera is woken once, just until its
// first picture, which is kept; with a picture already, never.
func TestFirstPictureIsTakenOnceAndKept(t *testing.T) {
	fastWake(t)
	dir := t.TempDir()
	var wakes int32
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(&wakes, 1)
		h.SetKeyframe("H264", []byte{0, 0, 0, 1, 0x65})
		<-ctx.Done()
		return nil
	}
	s := New([]config.Camera{battery}, run)
	s.SetStillDir(dir)
	start(t, s)
	waitFor(t, func() bool {
		_, e1 := os.Stat(filepath.Join(dir, "porch_sub.still"))
		_, e2 := os.Stat(filepath.Join(dir, "porch_main.still"))
		return e1 == nil && e2 == nil
	})
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&wakes); n != 2 {
		t.Fatalf("woke %d times, want once per stream", n)
	}

	var again int32
	s2 := New([]config.Camera{battery}, func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(&again, 1)
		<-ctx.Done()
		return nil
	})
	s2.SetStillDir(dir)
	start(t, s2)
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&again); n != 0 {
		t.Fatalf("woke %d times with pictures already kept", n)
	}
}

// streamsWoken runs a session until it is canceled, recording which
// stream woke and whether any session is in progress.
func streamsWoken(woke chan<- string, awake *int32) Runner {
	return func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		woke <- cfg.Stream
		atomic.AddInt32(awake, 1)
		<-ctx.Done()
		atomic.AddInt32(awake, -1)
		return nil
	}
}

// Motion the camera reports elsewhere (reoevent's MQTT) wakes its main
// stream with nobody watching, so the presence stream carries the event
// live, and it sleeps again once the motion ends.
func TestBatteryCameraWakesForMotion(t *testing.T) {
	fastWake(t)
	woke := make(chan string, 8)
	var awake int32
	s := New([]config.Camera{battery}, streamsWoken(woke, &awake))
	start(t, s)

	if !s.Motion("porch", true) {
		t.Fatal("Motion did not find the battery camera")
	}
	select {
	case st := <-woke:
		if st != "main" {
			t.Fatalf("motion woke %s, want main", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("motion did not wake the camera")
	}
	s.Motion("porch", false)
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 0 })
	select {
	case st := <-woke:
		t.Fatalf("woke %s again after the motion ended", st)
	case <-time.After(200 * time.Millisecond):
	}
}

// Motion that never reports its end still lets the camera sleep.
func TestBatteryMotionWithNoEndStillSleeps(t *testing.T) {
	fastWake(t)
	hold := motionHold
	motionHold = 150 * time.Millisecond
	t.Cleanup(func() { motionHold = hold })
	woke := make(chan string, 8)
	var awake int32
	s := New([]config.Camera{battery}, streamsWoken(woke, &awake))
	start(t, s)

	s.Motion("porch", true)
	<-woke
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 0 })
}

func TestMotionIgnoresAWiredCamera(t *testing.T) {
	s := New([]config.Camera{{Name: "gate", Address: "192.0.2.1", Streams: []string{"main"}}}, counting(new(int32), new(int32)))
	if s.Motion("gate", true) {
		t.Fatal("Motion claimed a wired camera, which is never asleep")
	}
	if s.Motion("nope", true) {
		t.Fatal("Motion claimed a camera that does not exist")
	}
}

// always_on: a battery camera on constant power wakes with nobody
// watching and stays awake past the session limit.
func TestBatteryCameraAlwaysOnStaysAwake(t *testing.T) {
	fastWake(t)
	var wakes, awake int32
	cam := battery
	cam.AlwaysOn = true
	s := New([]config.Camera{cam}, counting(&wakes, &awake))
	start(t, s)

	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 1 })
	time.Sleep(3 * wakeMax)
	if atomic.LoadInt32(&awake) != 1 {
		t.Fatal("always-on camera went to sleep")
	}
	// sub and main each hold one session.
	if n := atomic.LoadInt32(&wakes); n != 2 {
		t.Fatalf("woke %d times; want one session per stream", n)
	}
}

// A battery camera that lists its streams stays on demand (#38): listing
// main and sub, with rtsp naming both, used to run them around the clock
// and keep the camera from ever sleeping.
func TestBatteryCameraListingStreamsStaysOnDemand(t *testing.T) {
	fastWake(t)
	cam := battery
	cam.Streams = []string{"main", "sub"}
	cam.RTSP = []string{"main", "sub"}
	var wakes, awake int32
	s := New([]config.Camera{cam}, counting(&wakes, &awake))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if wakes != 0 {
		t.Fatalf("woke %d times with nobody watching", wakes)
	}
	for _, st := range []string{"main", "sub"} {
		h, ok := s.Hub("porch/" + st)
		if !ok || !h.OnDemand() {
			t.Fatalf("listed %s stream is not on demand", st)
		}
	}
}

// main is always there for a battery camera, listed or not: the presence
// stream and motion wakes hang off it.
func TestBatteryCameraListingOnlySubKeepsMain(t *testing.T) {
	cam := battery
	cam.Streams = []string{"sub"}
	streams, onDemand := cameraStreams(cam)
	if !onDemand || !slices.Equal(streams, []string{"sub", "main"}) {
		t.Fatalf("got %v on demand %v, want [sub main] on demand", streams, onDemand)
	}
	if !slices.Equal(cam.Streams, []string{"sub"}) {
		t.Fatalf("cameraStreams changed the camera's own list: %v", cam.Streams)
	}
}
