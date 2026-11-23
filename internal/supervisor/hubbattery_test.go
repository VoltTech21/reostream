package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
	"github.com/VoltTech21/reostream/internal/config"
)

// A battery camera paired to a Home Hub: a channel of the hub, no UID.
var hubBattery = config.Camera{Name: "doorbell", Address: "192.0.2.20", Channel: 1,
	Battery: true, Streams: []string{"main", "sub"}, RTSP: []string{"main"}}

// #43: without battery = true such a camera was held open around the
// clock through the hub, which kept it awake and drained it in days.
func TestHubBatteryCameraSleepsWithNoViewer(t *testing.T) {
	fastWake(t)
	var wakes, awake int32
	s := New([]config.Camera{hubBattery}, counting(&wakes, &awake))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if wakes != 0 {
		t.Fatalf("woke %d times with nobody watching", wakes)
	}
	for _, st := range []string{"main", "sub"} {
		h, ok := s.Hub("doorbell/" + st)
		if !ok || !h.OnDemand() {
			t.Fatalf("hub battery camera's %s is not on demand", st)
		}
	}
}

func TestHubCameraWithoutBatteryStillRunsAroundTheClock(t *testing.T) {
	fastWake(t)
	cam := hubBattery
	cam.Battery = false
	streams, onDemand := cameraStreams(cam)
	if onDemand || len(streams) != 2 {
		t.Fatalf("cameraStreams = %v, %v; want main and sub, always on", streams, onDemand)
	}
}

func TestHubBatteryCameraWithNoStreamsRunsMainAndSubOnDemand(t *testing.T) {
	cam := hubBattery
	cam.Streams = nil
	streams, onDemand := cameraStreams(cam)
	if !onDemand || len(streams) != 2 {
		t.Fatalf("cameraStreams = %v, %v; want sub and main on demand", streams, onDemand)
	}
}

// The UDP search goes to the address, which behind a hub is the hub's,
// so it would only ever hear the hub.
func TestHubBatteryCameraIsNotSearched(t *testing.T) {
	fastWake(t)
	var searches int32
	wakeSearch = func(context.Context, string) (bcudp.Info, error) {
		atomic.AddInt32(&searches, 1)
		return bcudp.Info{}, context.Canceled
	}
	var wakes, awake int32
	s := New([]config.Camera{hubBattery}, counting(&wakes, &awake))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if n := atomic.LoadInt32(&searches); n != 0 {
		t.Fatalf("searched the hub's address %d times", n)
	}
}

// wake_on_rtsp: a reader of the presence stream wakes main as a viewer
// on the page does, and the camera sleeps once the reader leaves.
func TestRTSPReaderWakesWithWakeOnRTSP(t *testing.T) {
	fastWake(t)
	cam := hubBattery
	cam.WakeOnRTSP = true
	woke := make(chan string, 8)
	var awake, readers int32
	s := New([]config.Camera{cam}, streamsWoken(woke, &awake))
	s.AttachRTSPReaders(func(camera string) int {
		if camera != "doorbell" {
			return 0
		}
		return int(atomic.LoadInt32(&readers))
	})
	start(t, s)

	select {
	case st := <-woke:
		t.Fatalf("woke %s with no reader", st)
	case <-time.After(150 * time.Millisecond):
	}
	atomic.StoreInt32(&readers, 1)
	select {
	case st := <-woke:
		if st != "main" {
			t.Fatalf("an RTSP reader woke %s, want main", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an RTSP reader did not wake the camera")
	}
	atomic.StoreInt32(&readers, 0)
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 0 })
}

// Without wake_on_rtsp a recorder holding the presence stream open must
// not keep the camera awake: that is what the presence stream is for.
func TestRTSPReaderDoesNotWakeWithoutWakeOnRTSP(t *testing.T) {
	fastWake(t)
	woke := make(chan string, 8)
	var awake int32
	s := New([]config.Camera{hubBattery, battery}, streamsWoken(woke, &awake))
	s.AttachRTSPReaders(func(string) int { return 1 })
	start(t, s)
	select {
	case st := <-woke:
		t.Fatalf("woke %s for a reader without wake_on_rtsp", st)
	case <-time.After(300 * time.Millisecond):
	}
}
