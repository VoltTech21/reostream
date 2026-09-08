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
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	n := atomic.LoadInt32(&attempts)
	if n < 2 {
		t.Fatalf("attempted %d times, expected a retry", n)
	}
	// Backoff must actually back off. Without it a refused camera becomes a
	// tight reconnect loop that hammers the camera and fills the log.
	if n > 12 {
		t.Fatalf("attempted %d times in 700ms, backoff is not being applied", n)
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
