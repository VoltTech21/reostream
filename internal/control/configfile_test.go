package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const validTOML = `listen = "0.0.0.0:8560"

[[camera]]
name = "one"
address = "192.0.2.50"
streams = ["main"]
`

func TestSaveConfigWritesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveConfig(path, validTOML); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validTOML {
		t.Fatalf("config not written; got %q", got)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("no backup kept: %v", err)
	}
	if !strings.Contains(string(backup), "0.0.0.0:1") {
		t.Fatalf("backup holds %q, not the previous config", backup)
	}
}

func TestSaveConfigRejectsAnUnknownKeyAndLeavesTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(validTOML), 0o600)

	err := saveConfig(path, validTOML+"\nnonsense = 1\n")
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != validTOML {
		t.Fatal("a rejected save modified the file")
	}
}

func TestSaveConfigRejectsAnInvalidCamera(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(validTOML), 0o600)

	err := saveConfig(path, `[[camera]]
name = "one"
streams = ["main"]
`)
	if err == nil {
		t.Fatal("a camera with no address was accepted")
	}
	if !strings.Contains(err.Error(), "address") {
		t.Fatalf("error %q does not name the problem", err)
	}
}

func TestSaveConfigReportsTheRealPathNotTheTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(validTOML), 0o600)

	err := saveConfig(path, validTOML+"\nnonsense = 1\n")
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the config path %q", err, path)
	}
	if strings.Contains(err.Error(), ".reostream-config-") {
		t.Fatalf("error %q leaks the temp file name", err)
	}
}

func TestSaveConfigPreservesTheTargetsExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:1\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := saveConfig(path, validTOML); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v, want 0640 preserved", info.Mode().Perm())
	}
}

func TestSaveConfigFallsBackToInPlaceWriteWhenRenameIsBusy(t *testing.T) {
	// A single-file bind mount makes the real os.Rename return EBUSY; that
	// cannot be reproduced with a real mount in a test, so renameConfig is
	// overridden to return the same error rename would give under a bind
	// mount, and the fallback writer is exercised directly through
	// saveConfig from there on.
	old := renameConfig
	renameConfig = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EBUSY}
	}
	defer func() { renameConfig = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:1\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := saveConfig(path, validTOML); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validTOML {
		t.Fatalf("in-place fallback did not write the new config; got %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("in-place fallback changed mode to %v, want 0640 preserved", info.Mode().Perm())
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("no backup kept before the in-place fallback: %v", err)
	}
	if !strings.Contains(string(backup), "0.0.0.0:1") {
		t.Fatalf("backup holds %q, not the previous config", backup)
	}
}

func TestSaveConfigDoesNotFallBackOnAnUnrelatedRenameError(t *testing.T) {
	old := renameConfig
	renameConfig = func(oldpath, newpath string) error {
		return fmt.Errorf("rename %s: %w", oldpath, syscall.EACCES)
	}
	defer func() { renameConfig = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(validTOML), 0o640); err != nil {
		t.Fatal(err)
	}

	err := saveConfig(path, `listen = "0.0.0.0:9999"
`)
	if err == nil {
		t.Fatal("expected the permission error to surface, not trigger a fallback")
	}
	got, _ := os.ReadFile(path)
	if string(got) != validTOML {
		t.Fatal("a save that should have failed cleanly modified the file")
	}
}
