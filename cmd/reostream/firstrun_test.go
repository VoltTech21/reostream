package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestStartupWithAnExplicitMissingConfigStaysFatal(t *testing.T) {
	// A -config that names a file which is not there is a broken
	// deployment (unmounted volume, typo'd flag, migration in progress),
	// not a fresh install: it must still refuse to start, exactly as it
	// always has, rather than quietly booting an unauthenticated control
	// page on the default first-run config.
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.toml")
	if _, err := startup(missing, dir, "", ""); err == nil {
		t.Fatal("expected an error for an explicit -config pointing at a missing file")
	}
}

func TestStartupWithNoConfigSynthesizesAndServes(t *testing.T) {
	// The whole point of the no-config path: with nothing at
	// <data>/config.toml and no -config given, the daemon must actually
	// come up -- an empty fleet, and a control listener that answers --
	// not merely fail to crash.
	dir := t.TempDir()
	d, err := startup("", dir, "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.controlSrv != nil {
			d.controlSrv.Close()
		}
		d.httpSrv.Close()
	})

	if len(d.cfg.Cameras) != 0 {
		t.Fatalf("synthesized config has %d cameras, want 0", len(d.cfg.Cameras))
	}
	if d.controlSrv == nil {
		t.Fatal("startup did not start a control server for a synthesized config")
	}

	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Get("http://127.0.0.1:8562/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control listener never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("control page returned %d", resp.StatusCode)
	}
}

func TestFirstRunClaimedTracksTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if firstRunClaimed(path) {
		t.Fatal("claimed before the file exists")
	}
	if err := os.WriteFile(path, []byte(`listen = "0.0.0.0:8560"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !firstRunClaimed(path) {
		t.Fatal("not claimed after the file was written")
	}
}
