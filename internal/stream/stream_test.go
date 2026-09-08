package stream

import (
	"context"
	"testing"
	"time"
)

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An already cancelled context must return promptly rather than dialling.
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Name: "x", Address: "192.0.2.1:9000", Stream: "main"}, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run ignored a cancelled context")
	}
}

func TestPingIsNotSelectedAgainstTheFrameChannel(t *testing.T) {
	// Frames arrive about every 40ms, so a timer case in the same select as
	// the frame case is effectively never chosen and the camera times the
	// session out. Run instead checks elapsed time after each message; this
	// exercises that check, shouldPing, directly rather than through a real
	// connection and a real clock.
	now := time.Now()
	if shouldPing(now, now.Add(time.Second), 10*time.Second) {
		t.Error("pinged after 1s with a 10s interval")
	}
	if !shouldPing(now, now.Add(11*time.Second), 10*time.Second) {
		t.Error("did not ping after 11s with a 10s interval")
	}
}
