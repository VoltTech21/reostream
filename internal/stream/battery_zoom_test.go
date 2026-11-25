package stream

import (
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakenvr"
	"github.com/VoltTech21/reostream/internal/hub"
)

// A battery camera's recorder zoom gets a connection of its own. Closing that
// viewer must close the recorder connection too: some recorders acknowledge a
// stop for externStream but keep the child camera awake until TCP closes.
func TestBatteryRecorderZoomUsesDisposableConnection(t *testing.T) {
	SetRecorderSharing(baichuan.DefaultMaxPreviews)
	defer SetRecorderSharing(baichuan.DefaultMaxPreviews)
	nvr := fakenvr.New(t, h265Fixture(t))

	mainCtx, cancelMain := context.WithCancel(context.Background())
	defer cancelMain()
	mainHub := hub.New(64)
	mainCh, cancelMainSub := mainHub.Subscribe()
	defer cancelMainSub()
	go Run(mainCtx, Config{Name: "wired", Address: nvr.Addr(), Channel: 1, Stream: "main"}, mainHub)
	waitForChunk(t, "wired main", mainCh)

	zoomCtx, cancelZoom := context.WithCancel(context.Background())
	zoomHub := hub.New(64)
	zoomCh, cancelZoomSub := zoomHub.Subscribe()
	defer cancelZoomSub()
	go Run(zoomCtx, Config{Name: "battery", Address: nvr.Addr(), Channel: 6, Stream: "zoom", Sleeps: true}, zoomHub)
	waitForChunk(t, "battery zoom", zoomCh)

	if got := nvr.Accepts(); got != 2 {
		t.Fatalf("recorder accepted %d connections, want 2", got)
	}
	want := fakenvr.Preview{Channel: 6, Handle: 1024 + 6, Stream: baichuan.StreamExtern}
	found := false
	for _, p := range nvr.Starts() {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("previews asked for = %v, want %v", nvr.Starts(), want)
	}

	cancelZoom()
	for deadline := time.Now().Add(2 * time.Second); nvr.Open() != 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if got := nvr.Open(); got != 1 {
		t.Fatalf("open recorder connections after zoom stopped = %d, want wired main only", got)
	}
}
