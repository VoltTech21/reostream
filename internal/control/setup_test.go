package control

import (
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

func TestRecorderURLsCoverBothOutputs(t *testing.T) {
	cfg := &config.Config{
		Listen: "0.0.0.0:8560",
		RTSP:   &config.RTSPConfig{Listen: "0.0.0.0:8561"},
		Cameras: []config.Camera{{
			Name: "driveway", Address: "192.0.2.50",
			Streams: []string{"main", "sub"}, RTSP: []string{"main"},
		}},
	}
	got := recorderURLs(cfg, "reostream")

	if !strings.Contains(got.Frigate, "http://reostream:8560/driveway_sub.ts") {
		t.Fatalf("frigate block lacks the detect stream:\n%s", got.Frigate)
	}
	if !strings.Contains(got.Frigate, "http://reostream:8560/driveway.ts") {
		t.Fatalf("frigate block lacks the record stream:\n%s", got.Frigate)
	}
	if len(got.RTSP) != 1 || got.RTSP[0] != "rtsp://reostream:8561/driveway" {
		t.Fatalf("rtsp urls are %v", got.RTSP)
	}
}

func TestRecorderURLsOmitRTSPWhenItIsOff(t *testing.T) {
	cfg := &config.Config{
		Listen:  "0.0.0.0:8560",
		Cameras: []config.Camera{{Name: "a", Address: "x", Streams: []string{"main"}}},
	}
	if got := recorderURLs(cfg, "host"); len(got.RTSP) != 0 {
		t.Fatalf("got %v, want no rtsp urls when there is no [rtsp] section", got.RTSP)
	}
}
