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

// With a login on the video outputs, every URL a recorder is handed carries
// it: one pasted without it is a 401 that looks like a broken stream.
func TestRecorderURLsCarryTheLogin(t *testing.T) {
	cfg := &config.Config{
		Listen: "0.0.0.0:8560",
		RTSP:   &config.RTSPConfig{Listen: "0.0.0.0:8561", Username: "frigate", Password: "s3cret"},
		Cameras: []config.Camera{{
			Name: "driveway", Address: "192.0.2.50",
			Streams: []string{"main", "sub"}, RTSP: []string{"main", "sub"},
		}},
	}
	got := recorderURLs(cfg, "reostream")

	for _, want := range []string{
		"http://frigate:s3cret@reostream:8560/driveway_sub.ts",
		"http://frigate:s3cret@reostream:8560/driveway.ts",
	} {
		if !strings.Contains(got.Frigate, want) {
			t.Errorf("frigate block lacks %s:\n%s", want, got.Frigate)
		}
	}
	for _, in := range got.FrigateInputs["driveway"] {
		if !strings.Contains(in.Path, "frigate:s3cret@") {
			t.Errorf("frigate input %s has no login, and the config updater pastes these", in.Path)
		}
	}
	if u := got.StreamHTTP["driveway"]["sub"]; u != "http://frigate:s3cret@reostream:8560/driveway_sub.ts" {
		t.Errorf("camera page HTTP url is %s", u)
	}
	if u := got.StreamRTSP["driveway"]["main"]; u != "rtsp://frigate:s3cret@reostream:8561/driveway" {
		t.Errorf("camera page RTSP url is %s", u)
	}
	for _, u := range append(append([]string(nil), got.RTSP...), got.HTTP...) {
		if !strings.Contains(u, "frigate:s3cret@") {
			t.Errorf("%s has no login", u)
		}
	}
	if got.LoginUser != "frigate" || got.LoginPassword != "s3cret" {
		t.Errorf("login for Blue Iris is %q, %q", got.LoginUser, got.LoginPassword)
	}
}

// The URL pages read the config raw, with "$NAME" unresolved; the login in
// the URLs is the real one, and a password URLs cannot carry as written
// is escaped.
func TestRecorderURLsResolveAndEscapeTheLogin(t *testing.T) {
	t.Setenv("TEST_STREAM_PASS", "p@ss/word")
	cfg := &config.Config{
		Listen:  "0.0.0.0:8560",
		RTSP:    &config.RTSPConfig{Listen: "0.0.0.0:8561", Username: "frigate", Password: "$TEST_STREAM_PASS"},
		Cameras: []config.Camera{{Name: "a", Address: "x", Streams: []string{"main"}, RTSP: []string{"main"}}},
	}
	got := recorderURLs(cfg, "host")
	if len(got.RTSP) != 1 || got.RTSP[0] != "rtsp://frigate:p%40ss%2Fword@host:8561/a" {
		t.Fatalf("rtsp urls are %v", got.RTSP)
	}
	if got.LoginPassword != "p@ss/word" {
		t.Fatalf("login password is %q, want the resolved one", got.LoginPassword)
	}
}

// The Frigate config updater writes the same login into a pasted config.
func TestMergedFrigateConfigCarriesTheLogin(t *testing.T) {
	cfg := &config.Config{
		Listen:  "0.0.0.0:8560",
		RTSP:    &config.RTSPConfig{Listen: "0.0.0.0:8561", Username: "frigate", Password: "s3cret"},
		Cameras: []config.Camera{{Name: "a", Address: "x", Streams: []string{"main", "sub"}}},
	}
	u := recorderURLs(cfg, "host")
	out, _, _, err := mergeFrigate([]byte("cameras:\n  a:\n    ffmpeg:\n      inputs:\n        - path: rtsp://old/a\n"), u.FrigateInputs, u.Cameras)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "http://frigate:s3cret@host:8560/a_sub.ts") {
		t.Fatalf("merged config has no login:\n%s", out)
	}
}
