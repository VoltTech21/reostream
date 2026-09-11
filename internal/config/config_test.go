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

func TestTwoCamerasSharingAnAddressOnAnOverlappingStreamAreRejected(t *testing.T) {
	// Different names, same physical camera, both asking for main: two
	// runners would fight for the same connection, exactly the fault this
	// daemon exists to prevent (a camera permits one connection per
	// stream, and the loser sits locked out for minutes).
	cfg := &Config{Cameras: []Camera{
		{Name: "front", Address: "192.0.2.1", Streams: []string{"main", "sub"}},
		{Name: "front-again", Address: "192.0.2.1", Streams: []string{"main"}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected two cameras sharing an address and a stream to be rejected")
	}
}

func TestTwoCamerasSharingAnAddressOnDisjointStreamsAreAllowed(t *testing.T) {
	// Safe on purpose: a real camera's main, sub and extern are already
	// independent Baichuan connections (see the supervisor package), so
	// splitting them across two config entries with different names is no
	// different from one entry listing all three. There is nothing here to
	// fight over.
	cfg := &Config{Cameras: []Camera{
		{Name: "front-main", Address: "192.0.2.1", Streams: []string{"main"}},
		{Name: "front-sub", Address: "192.0.2.1", Streams: []string{"sub"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected disjoint streams on a shared address to be allowed, got: %v", err)
	}
}

func TestSharedAddressCheckNormalisesThePort(t *testing.T) {
	// "192.0.2.1" and "192.0.2.1:9000" name the same camera on the wire
	// (see NormalizeAddr / baichuan.Dial's own default), so this must be
	// caught the same as an exact string match would be.
	cfg := &Config{Cameras: []Camera{
		{Name: "front", Address: "192.0.2.1", Streams: []string{"main"}},
		{Name: "front-again", Address: "192.0.2.1:9000", Streams: []string{"main"}},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected the default-port form to be recognised as the same address")
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

// This is the exact fault the real deployment hit: reocam's config has
// [control].password set to a variable ("$REOSTREAM_CONTROL_PASSWORD")
// that lives in the streaming daemon's own container, not reocam's, so
// Load failed at every request and the camera control page returned 500
// across the board. LoadForDialing must load the fleet anyway, because it
// never uses [control].password at all.
func TestLoadForDialingIgnoresAnUnresolvableControlPassword(t *testing.T) {
	os.Unsetenv("TEST_DIALING_UNSET_CONTROL_PW")
	t.Setenv("TEST_DIALING_CAM_PASSWORD", "s3cret")

	cfg, err := LoadForDialing("testdata/dialing.toml")
	if err != nil {
		t.Fatalf("LoadForDialing: %v", err)
	}
	if got := cfg.Cameras[0].Password; got != "s3cret" {
		t.Fatalf("camera password = %q, want it resolved from the environment", got)
	}
	if got := cfg.Control.Password; got != "$TEST_DIALING_UNSET_CONTROL_PW" {
		t.Fatalf("control password = %q, want the raw reference left unresolved", got)
	}
}

// A camera password must still fail loudly when unset: a camera that
// silently never authenticates is much harder to notice than a process
// that refuses to start, and LoadForDialing must not relax that half.
func TestLoadForDialingStillRequiresCameraPasswords(t *testing.T) {
	os.Unsetenv("TEST_DIALING_UNSET_CONTROL_PW")
	os.Unsetenv("TEST_DIALING_CAM_PASSWORD")

	if _, err := LoadForDialing("testdata/dialing.toml"); err == nil {
		t.Fatal("expected an error when a camera's referenced variable is unset")
	}
}

// Load, unlike LoadForDialing, genuinely needs [control].password: it is
// what the streaming daemon itself starts its own control listener with.
func TestLoadStillRequiresControlPassword(t *testing.T) {
	os.Unsetenv("TEST_DIALING_UNSET_CONTROL_PW")
	t.Setenv("TEST_DIALING_CAM_PASSWORD", "s3cret")

	if _, err := Load("testdata/dialing.toml"); err == nil {
		t.Fatal("expected Load to fail when [control].password's variable is unset")
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
