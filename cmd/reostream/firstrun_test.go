package main

import "testing"

func TestConfigPathPrefersAnExplicitOverride(t *testing.T) {
	// An existing deployment passes -config and must keep working
	// unchanged; only a fresh install gets the data directory convention.
	if got := resolveConfigPath("/data", "/etc/reostream/config.toml"); got != "/etc/reostream/config.toml" {
		t.Fatalf("got %q, want the override", got)
	}
	if got := resolveConfigPath("/data", ""); got != "/data/config.toml" {
		t.Fatalf("got %q, want the data directory default", got)
	}
}
