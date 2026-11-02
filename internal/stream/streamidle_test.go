package stream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
)

// A camera that was sending video and goes silent is let go after
// streamingIdleTimeout, not the 15 s idle timeout a connection starts with:
// a 5 s link blip used to cost about 17 s of footage (a link-drop test,
// 10-30). The fixture is a few seconds of video, then nothing.
func TestAStreamingCameraThatGoesSilentIsLetGoQuickly(t *testing.T) {
	old := streamingIdleTimeout
	streamingIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { streamingIdleTimeout = old })

	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(100000)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Name: "test", Address: cam.Addr(), Username: "admin", Stream: "main"}, h)
	}()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no video")
	}
	go func() {
		for range ch {
		}
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "idle timeout") {
			t.Errorf("Run returned %v, want the idle timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run still waiting 5 s after the camera went silent")
	}
}
