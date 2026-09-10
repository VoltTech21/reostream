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

func TestRecorderURLsBracketALiteralIPv6Host(t *testing.T) {
	// A bare "%s:%s" interpolation of a literal IPv6 host produces
	// "http://fe80::1:8560/...", which nothing parses as that host and
	// that port: the extra colons all look like part of the address.
	// net.JoinHostPort is what actually brackets it.
	cfg := &config.Config{
		Listen:  "[::]:8560",
		RTSP:    &config.RTSPConfig{Listen: "[::]:8561"},
		Cameras: []config.Camera{{Name: "a", Address: "x", Streams: []string{"main"}, RTSP: []string{"main"}}},
	}
	got := recorderURLs(cfg, "fe80::1")
	if !strings.Contains(got.Frigate, "http://[fe80::1]:8560/a.ts") {
		t.Fatalf("frigate block does not bracket the IPv6 host:\n%s", got.Frigate)
	}
	if len(got.RTSP) != 1 || got.RTSP[0] != "rtsp://[fe80::1]:8561/a" {
		t.Fatalf("rtsp urls are %v, want the IPv6 host bracketed", got.RTSP)
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
