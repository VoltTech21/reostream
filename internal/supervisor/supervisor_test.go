package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	var live, maxLive int32
	var mu sync.Mutex
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		n := atomic.AddInt32(&live, 1)
		mu.Lock()
		if n > maxLive {
			maxLive = n
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&live, -1)
		return errors.New("drop")
	}
	s := New([]config.Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}}}, run)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if maxLive > 1 {
		t.Fatalf("%d concurrent connections to one stream, want at most 1", maxLive)
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
