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
