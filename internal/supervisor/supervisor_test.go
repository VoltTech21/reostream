package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

func TestRestartsAFailedStreamWithBackoff(t *testing.T) {
	var attempts int32
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("connection refused")
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	// The production backoff starts at 1s deliberately (see the comment on
	// defaultBackoffStart), which would make this test either slow or
	// unable to observe more than one attempt. setBackoff exists so tests
	// can use a short, deterministic value instead.
	s.setBackoff(20*time.Millisecond, 15*time.Second, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	n := atomic.LoadInt32(&attempts)
	if n < 2 {
		t.Fatalf("attempted %d times, expected a retry", n)
	}
	// The upper bound is the half that matters: doubling 20ms across 300ms
	// caps out at 4-5 attempts (20+40+80+160 already exceeds the window),
	// so a much higher count means the delay is not actually growing and
	// the camera would be hammered with no backoff at all.
	if n > 8 {
		t.Fatalf("attempted %d times in 300ms, backoff is not being applied", n)
	}
}

func TestDefaultBackoffStartIsOneSecond(t *testing.T) {
	// Regression guard: the backoff start was once quietly shortened to
	// make a test pass instead of making the test inject its own value.
	// The 1s default is load-bearing, not a style choice: the common cause
	// of a refused connection is the camera still holding a session from a
	// client that died without sending stream-stop, and that clears in
	// minutes, so retrying faster than 1s only adds load and log noise
	// without any chance of succeeding sooner.
	s := New(nil, nil)
	if s.backoffStart != time.Second {
		t.Fatalf("default backoffStart = %v, want 1s", s.backoffStart)
	}
}

func TestOneStreamFailingDoesNotDisturbAnother(t *testing.T) {
	var healthy int32
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		if cfg.Name == "bad" {
			return errors.New("nope")
		}
		atomic.AddInt32(&healthy, 1)
		<-ctx.Done()
		return ctx.Err()
	}
	s := New([]config.Camera{
		{Name: "bad", Address: "192.0.2.1", Streams: []string{"main"}},
		{Name: "good", Address: "192.0.2.2", Streams: []string{"main"}},
	}, run)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if atomic.LoadInt32(&healthy) != 1 {
		t.Fatal("the healthy stream was started more than once, or not at all")
	}
}

func TestNeverRunsTwoConnectionsForOneStream(t *testing.T) {
	// The rule this whole project turns on. A restart must not overlap with
	// the connection it replaces: a second connection to one stream gets a
	// session that delivers nothing and blocks its own replacement.
	//
	// The production 1s backoff would only fit one or two restart cycles
	// into a test-sized window, which observes no violation without coming
	// close to proving one is impossible. setBackoff drives this down to
	// 1ms (start and cap both, so the interval stays flat) to force dozens
	// of restart cycles and give the invariant a real chance to fail.
	var live, maxLive, restarts int32
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		atomic.AddInt32(&restarts, 1)
		n := atomic.AddInt32(&live, 1)
		for {
			old := atomic.LoadInt32(&maxLive)
			if n <= old {
				break
			}
			if atomic.CompareAndSwapInt32(&maxLive, old, n) {
				break
			}
		}
		time.Sleep(1 * time.Millisecond)
		atomic.AddInt32(&live, -1)
		return errors.New("drop")
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	s.setBackoff(1*time.Millisecond, 1*time.Millisecond, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	if n := atomic.LoadInt32(&restarts); n < 20 {
		t.Fatalf("only %d restart cycles in 500ms, too few to trust this test caught a violation", n)
	}
	if got := atomic.LoadInt32(&maxLive); got > 1 {
		t.Fatalf("%d concurrent connections to one stream, want at most 1", got)
	}
}

func TestRunReturnsErrorOnSecondCall(t *testing.T) {
	// Two concurrent Run calls would spawn two goroutines per entry, which
	// is a second connection to every stream the Supervisor owns: the same
	// fault TestNeverRunsTwoConnectionsForOneStream guards against, caused
	// by the caller instead of a flaky camera.
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(firstDone)
	}()

	// Give the first Run a chance to actually mark itself started before
	// the second call races it; CompareAndSwap makes the outcome correct
	// either way, but this keeps the test from depending on that timing.
	time.Sleep(20 * time.Millisecond)

	if err := s.Run(ctx); err != ErrAlreadyRunning {
		t.Fatalf("second Run() = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	<-firstDone
}

func TestRestartsCountReflectsActualRestartsNotFirstRun(t *testing.T) {
	// A stream that runs once and is still running when the context ends
	// has never restarted, and Stats().Restarts should say 0, not 1: that
	// number gets read by an operator deciding whether a camera is flapping.
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	stats := s.Stats()
	if len(stats) != 1 {
		t.Fatalf("got %d stream stats, want 1", len(stats))
	}
	if stats[0].Restarts != 0 {
		t.Fatalf("Restarts = %d for a stream that never failed, want 0", stats[0].Restarts)
	}
}

func TestRunReturnsOnlyAfterEveryStreamHasStopped(t *testing.T) {
	// Shutdown must not race the stream close, because the stop message is
	// what releases the camera's session.
	stopped := make(chan struct{})
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(stopped)
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	s.Run(ctx)
	select {
	case <-stopped:
	default:
		t.Fatal("Run returned before the stream finished closing")
	}
}

// A camera with no rtsp entry gets no sink, which is the state the HTTP path
// has always run in.
func TestNoSinkWithoutRTSPConfig(t *testing.T) {
	s := New([]config.Camera{{
		Name: "a", Address: "192.0.2.1", Streams: []string{"main"},
	}}, nil)
	s.AttachSinks(func(camera, st string) stream.FrameSink {
		t.Errorf("factory called for %s/%s, which has no rtsp entry", camera, st)
		return nil
	})
	if s.entries["a/main"].cfg.Sink != nil {
		t.Error("a camera without rtsp config got a sink")
	}
}

// Only the streams named in rtsp get a sink, not every stream the camera
// pulls.
func TestSinkOnlyForNamedStreams(t *testing.T) {
	s := New([]config.Camera{{
		Name: "a", Address: "192.0.2.1",
		Streams: []string{"main", "sub"},
		RTSP:    []string{"sub"},
	}}, nil)
	s.AttachSinks(func(_, st string) stream.FrameSink {
		return stubSink{}
	})
	for _, e := range s.entries {
		want := e.cfg.Stream == "sub"
		if got := e.cfg.Sink != nil; got != want {
			t.Errorf("%s: sink=%v, want %v", e.cfg.Stream, got, want)
		}
	}
}

type stubSink struct{}

func (stubSink) Frame(baichuan.Frame) {}

func TestStopEntryWaitsForTheStreamToReturn(t *testing.T) {
	released := make(chan struct{})
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		// Stands in for the stream-stop message: a camera whose session is
		// not released refuses its next connection for minutes, so stopEntry
		// must not return before this has happened.
		close(released)
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "a", Address: "x", Streams: []string{"main"}}}, run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, func() bool { return s.Stats()[0].Running })

	s.mu.Lock()
	e := s.entries["a/main"]
	s.mu.Unlock()
	s.stopEntry(e)

	select {
	case <-released:
	default:
		t.Fatal("stopEntry returned before the stream released its session")
	}
}

func TestRunWithNoStreamsServesAndStops(t *testing.T) {
	s := New(nil, func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with an empty fleet did not return")
	}
}

// waitFor polls cond for up to a second, which is long enough for a
// goroutine to be scheduled and short enough to fail a hung test quickly.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 1s")
}
