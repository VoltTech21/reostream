package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRejectsUnknownKeys(t *testing.T) {
	// A typo in a camera name should fail loudly at boot, not serve 404s at
	// three in the morning.
	_, err := Load("testdata/unknown_key.toml")
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "adress") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
}

func TestPasswordCanComeFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_CAM_PASSWORD", "s3cret")
	c, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Cameras[0].Password; got != "s3cret" {
		t.Fatalf("password = %q, want it resolved from the environment", got)
	}
}

func TestMissingEnvironmentPasswordIsAnError(t *testing.T) {
	// Failing at boot is better than a camera that silently never connects.
	os.Unsetenv("TEST_CAM_PASSWORD")
	if _, err := Load("testdata/valid.toml"); err == nil {
		t.Fatal("expected an error when the referenced variable is unset")
	}
}

func TestLoadRawLeavesAnEnvironmentReferenceUnresolved(t *testing.T) {
	// LoadRaw must not need the referenced variable set at all, and must
	// not resolve it even when it is.
	t.Setenv("TEST_CAM_PASSWORD", "s3cret")
	c, err := LoadRaw("testdata/valid.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Cameras[0].Password; got != "$TEST_CAM_PASSWORD" {
		t.Fatalf("password = %q, want the raw \"$TEST_CAM_PASSWORD\" reference untouched", got)
	}
}

func TestLoadRawStillRejectsUnknownKeys(t *testing.T) {
	_, err := LoadRaw("testdata/unknown_key.toml")
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "adress") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
}

func TestDuplicateStreamIsRejected(t *testing.T) {
	// A camera permits one connection per stream. Two entries for the same
	// camera and stream would have them fight, and the loser blocks the winner
	// until the camera times the session out.
	cfg := &Config{Cameras: []Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"main", "main"}}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate stream to be rejected")
	}
}

func TestDuplicateNameIsRejected(t *testing.T) {
	cfg := &Config{Cameras: []Camera{
		{Name: "a", Address: "192.0.2.1", Streams: []string{"main"}},
		{Name: "a", Address: "192.0.2.2", Streams: []string{"main"}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate camera name to be rejected")
	}
}

func TestUnknownStreamNameIsRejected(t *testing.T) {
	cfg := &Config{Cameras: []Camera{{Name: "a", Address: "192.0.2.1", Streams: []string{"hi-res"}}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an unknown stream name to be rejected")
	}
}

func TestLoadRTSPSection(t *testing.T) {
	cfg, err := Load("testdata/rtsp.toml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RTSP == nil {
		t.Fatal("RTSP section not decoded")
	}
	if cfg.RTSP.Listen != "0.0.0.0:8554" {
		t.Errorf("listen = %q", cfg.RTSP.Listen)
	}
	if got := cfg.Cameras[0].RTSP; len(got) != 1 || got[0] != "main" {
		t.Errorf("camera rtsp = %v, want [main]", got)
	}
}

// Absent section means RTSP is off, which is the default posture.
func TestRTSPAbsentMeansOff(t *testing.T) {
	t.Setenv("TEST_CAM_PASSWORD", "x")
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RTSP != nil {
		t.Error("RTSP should be nil when the section is absent")
	}
}

// Serving a stream the camera is not pulling would publish a URL that never
// carries a frame, which is the kind of thing found at three in the morning.
func TestRTSPStreamMustBePulled(t *testing.T) {
	cfg := Config{
		Listen: ":8560",
		RTSP:   &RTSPConfig{Listen: ":8554"},
		Cameras: []Camera{{
			Name: "a", Address: "192.0.2.1", Username: "admin",
			Streams: []string{"main"}, RTSP: []string{"sub"},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for an rtsp stream that is not pulled")
	}
}

// An rtsp entry with no listener configured cannot be served either.
func TestRTSPWithoutListenerIsAnError(t *testing.T) {
	cfg := Config{
		Listen: ":8560",
		Cameras: []Camera{{
			Name: "a", Address: "192.0.2.1", Username: "admin",
			Streams: []string{"main"}, RTSP: []string{"main"},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for rtsp with no [rtsp] section")
	}
}

func TestRTSPUnknownStreamName(t *testing.T) {
	cfg := Config{
		Listen: ":8560",
		RTSP:   &RTSPConfig{Listen: ":8554"},
		Cameras: []Camera{{
			Name: "a", Address: "192.0.2.1", Username: "admin",
			Streams: []string{"main"}, RTSP: []string{"quad"},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error for an unknown rtsp stream name")
	}
}

func TestControlPasswordComesFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_CONTROL_PW", "hunter2")
	cfg, err := Load("testdata/control.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Control == nil {
		t.Fatal("no [control] section decoded")
	}
	if cfg.Control.Password != "hunter2" {
		t.Fatalf("password is %q, want the resolved environment value", cfg.Control.Password)
	}
}

func TestControlWithoutAPasswordIsRefused(t *testing.T) {
	cfg := Config{
		Control: &ControlConfig{Listen: "0.0.0.0:8562"},
		Cameras: []Camera{{Name: "a", Address: "x", Streams: []string{"main"}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a control listener with no password was accepted")
	}
	if !strings.Contains(err.Error(), "allow_no_password") {
		t.Fatalf("error %q does not say how to proceed deliberately", err)
	}
}

func TestControlWithoutAPasswordIsAllowedWhenSaidExplicitly(t *testing.T) {
	cfg := Config{
		Control: &ControlConfig{Listen: "0.0.0.0:8562", AllowNoPassword: true},
		Cameras: []Camera{{Name: "a", Address: "x", Streams: []string{"main"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
