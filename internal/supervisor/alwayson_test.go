package supervisor

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

// #51: a powered doorbell reached by UID, kept always on, went through the
// presence stream from 0.2.9 on, which swaps its live sound for generated
// silence. Kept always on, it streams straight through like 0.2.8 did.
func TestAlwaysOnUIDCameraStreamsDirectly(t *testing.T) {
	cam := config.Camera{Name: "doorbell", UID: "95270000TEST0000", AlwaysOn: true,
		Streams: []string{"main"}, RTSP: []string{"main"}}
	streams, onDemand := cameraStreams(cam)
	if onDemand || len(streams) != 1 || streams[0] != "main" {
		t.Fatalf("cameraStreams = %v, %v; want main, always on", streams, onDemand)
	}
}

func TestUIDCameraNotAlwaysOnStillSleeps(t *testing.T) {
	cam := config.Camera{Name: "doorbell", UID: "95270000TEST0000",
		Streams: []string{"main"}, RTSP: []string{"main"}}
	if _, onDemand := cameraStreams(cam); !onDemand {
		t.Fatal("a UID camera not kept always on should stay on demand")
	}
}
