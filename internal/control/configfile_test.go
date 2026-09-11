package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
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

	if err := saveConfig(path, validTOML, nil); err != nil {
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

	err := saveConfig(path, validTOML+"\nnonsense = 1\n", nil)
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
`, nil)
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

	err := saveConfig(path, validTOML+"\nnonsense = 1\n", nil)
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

	if err := saveConfig(path, validTOML, nil); err != nil {
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

// TestSaveConfigDoesNotWriteWhenTheFleetCheckRejects is Finding 4 from the
// 2026-09-10 review: writeAndApply used to write the file first and only
// find out from Reload afterwards that the running fleet could not accept
// it, which is reachable (an [rtsp] section added to a daemon that booted
// without one passes config.Load but fails Reload's own RTSP-wiring
// check). checkFleet now runs before anything reaches disk.
func TestSaveConfigDoesNotWriteWhenTheFleetCheckRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(validTOML), 0o600)

	checkFleet := func(cams []config.Camera) error {
		return errors.New("simulated: rtsp is not wired up on this running supervisor")
	}
	err := saveConfig(path, validTOML, checkFleet)
	if err == nil {
		t.Fatal("expected the fleet check's rejection to surface")
	}
	got, _ := os.ReadFile(path)
	if string(got) != validTOML {
		t.Fatal("a config the fleet check rejected was still written to disk")
	}
	if _, statErr := os.Stat(path + ".bak"); statErr == nil {
		t.Fatal("a rejected save should not even reach the backup step")
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

	if err := saveConfig(path, validTOML, nil); err != nil {
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

// TestSaveConfigFallsBackWhenTheTargetDirectoryIsNotWritable reproduces the
// production failure: /etc/reostream is root-owned in the image, the
// daemon runs as nonroot, and config.toml is a single-file bind mount, so
// os.CreateTemp(filepath.Dir(path), ...) is denied before rename is ever
// reached. Root can write into a 0500 directory regardless of its mode, so
// this test cannot observe the failure it exists to catch when run as
// root; skip rather than let it pass for the wrong reason.
func TestSaveConfigFallsBackWhenTheTargetDirectoryIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits; this test needs a real non-writable directory")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:1\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	if err := saveConfig(path, validTOML, nil); err != nil {
		t.Fatalf("save should have fallen back to the system temp dir: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validTOML {
		t.Fatalf("config not written; got %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v, want 0640 preserved", info.Mode().Perm())
	}
	// The target directory refused config.toml.bak for the same reason it
	// refused the temp file, so the backup lands in the system temp dir
	// instead; see saveConfig's backupPath comment.
	backupPath := filepath.Join(os.TempDir(), "config.toml.bak")
	defer os.Remove(backupPath)
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("no backup kept at %s: %v", backupPath, err)
	}
	if !strings.Contains(string(backup), "0.0.0.0:1") {
		t.Fatalf("backup holds %q, not the previous config", backup)
	}
}

// TestSaveConfigReportsOwnershipMismatchWhenTheFileItselfIsUnwritable covers
// the other half of the bind-mount failure: the directory may be writable
// (system temp dir fallback succeeded, or rename was possible) while the
// mounted config file's own owner still does not match the container's
// nonroot user, so the in-place write is denied too. The message must say
// so plainly rather than surface a bare "permission denied".
func TestSaveConfigReportsOwnershipMismatchWhenTheFileItselfIsUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits; this test needs a real unwritable file")
	}

	// Rename would happily replace path regardless of path's own
	// permission bits, since a rename only needs the directory to be
	// writable, not the file it is displacing. So to reach writeInPlace at
	// all, force the EBUSY path exactly as
	// TestSaveConfigFallsBackToInPlaceWriteWhenRenameIsBusy does; only then
	// does opening the 0400 target for writing hit a real permission
	// error.
	old := renameConfig
	renameConfig = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EBUSY}
	}
	defer func() { renameConfig = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(validTOML), 0o400); err != nil {
		t.Fatal(err)
	}

	err := saveConfig(path, validTOML+"\n", nil)
	if err == nil {
		t.Fatal("expected the unwritable target to surface an error")
	}
	if !strings.Contains(err.Error(), "not writable by the daemon") {
		t.Fatalf("error %q does not explain the daemon cannot write the file", err)
	}
	if !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("error %q does not point at ownership as the likely cause", err)
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
`, nil)
	if err == nil {
		t.Fatal("expected the permission error to surface, not trigger a fallback")
	}
	got, _ := os.ReadFile(path)
	if string(got) != validTOML {
		t.Fatal("a save that should have failed cleanly modified the file")
	}
}
