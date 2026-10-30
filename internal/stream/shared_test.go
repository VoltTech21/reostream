package stream

import (
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakenvr"
	"github.com/VoltTech21/reostream/internal/hub"
)

func waitForChunk(t *testing.T, name string, ch <-chan []byte) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no frame reached the hub", name)
	}
}

// A TrackMix on an NVR's channel 1, with all four of its streams, as
// cam-19 runs: one connection, and two previews, since behind a recorder
// the zoom streams are channel 1's own main and sub.
func TestRunAllOfACamerasStreamsBehindAnNVROnOneConnection(t *testing.T) {
	nvr := fakenvr.New(t, h265Fixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, st := range []string{"main", "sub", "zoom", "zoom_sub"} {
		h := hub.New(64)
		ch, cancelSub := h.Subscribe()
		defer cancelSub()
		go Run(ctx, Config{Name: "behind-nvr", Address: nvr.Addr(), Channel: 1, Stream: st}, h)
		waitForChunk(t, st, ch)
	}
	if n := nvr.Accepts(); n != 1 {
		t.Fatalf("recorder accepted %d connections for four streams, want 1", n)
	}
	want := map[fakenvr.Preview]bool{
		{Channel: 1, Stream: baichuan.StreamMain}: true,
		{Channel: 1, Stream: baichuan.StreamSub}:  true,
	}
	got := nvr.Starts()
	if len(got) != len(want) {
		t.Fatalf("previews asked for = %v, want channel 1 main and sub once each", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("previews asked for = %v, want channel 1 main and sub once each", got)
		}
	}
}

// A camera behind the recorder that stops sending fails its own stream's
// media watchdog and nothing else: the other camera keeps streaming on the
// same connection, and the dark one comes back on it too.
func TestRunOneDarkChannelBehindAnNVRLeavesTheOthers(t *testing.T) {
	nvr := fakenvr.New(t, h265Fixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lit := hub.New(64)
	litCh, cancelLit := lit.Subscribe()
	defer cancelLit()
	go Run(ctx, Config{Name: "lit", Address: nvr.Addr(), Channel: 2, Stream: "sub"}, lit)
	waitForChunk(t, "lit", litCh)

	nvr.Silence(3, true)
	darkDone := make(chan error, 1)
	go func() {
		darkDone <- Run(ctx, Config{Name: "dark", Address: nvr.Addr(), Channel: 3, Stream: "sub", MediaTimeout: 300 * time.Millisecond}, hub.New(64))
	}()
	select {
	case err := <-darkDone:
		if err == nil || ctx.Err() != nil {
			t.Fatalf("dark stream returned %v, want its media watchdog's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dark stream's watchdog never fired")
	}
	// Drain what queued up while the dark stream ran, then check the lit
	// one is still live.
	for len(litCh) > 0 {
		<-litCh
	}
	waitForChunk(t, "lit after the dark channel failed", litCh)

	nvr.Silence(3, false)
	back := hub.New(64)
	backCh, cancelBack := back.Subscribe()
	defer cancelBack()
	go Run(ctx, Config{Name: "dark", Address: nvr.Addr(), Channel: 3, Stream: "sub"}, back)
	waitForChunk(t, "dark once lit again", backCh)
	if n := nvr.Accepts(); n != 1 {
		t.Fatalf("recorder accepted %d connections, want 1", n)
	}
}

// With sharing turned off every stream dials its own connection again.
func TestRunRecorderSharingOff(t *testing.T) {
	SetRecorderSharing(0)
	defer SetRecorderSharing(baichuan.DefaultMaxPreviews)
	nvr := fakenvr.New(t, h265Fixture(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, st := range []string{"main", "sub"} {
		h := hub.New(64)
		ch, cancelSub := h.Subscribe()
		defer cancelSub()
		go Run(ctx, Config{Name: "unshared", Address: nvr.Addr(), Channel: 1, Stream: st}, h)
		waitForChunk(t, st, ch)
	}
	if n := nvr.Accepts(); n != 2 {
		t.Fatalf("recorder accepted %d connections, want 2", n)
	}
}
