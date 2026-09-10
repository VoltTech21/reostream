package control

import (
	"os"
	"path/filepath"
	"strings"
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
