package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDataDir(t *testing.T) {
	old := containerData
	defer func() { containerData = old }()

	containerData = t.TempDir()
	if got := defaultDataDir(); got != containerData {
		t.Fatalf("with the container folder present = %q, want %q", got, containerData)
	}

	// A plain binary with no /data gets reostream-data where it was started (#57).
	containerData = filepath.Join(t.TempDir(), "missing")
	wd := t.TempDir()
	t.Chdir(wd)
	want := filepath.Join(wd, "reostream-data")
	if got := defaultDataDir(); got != want {
		t.Fatalf("with no container folder = %q, want %q", got, want)
	}

	// A file named like the folder is not the folder.
	f := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	containerData = f
	if got := defaultDataDir(); got != want {
		t.Fatalf("with a file at the container path = %q, want %q", got, want)
	}
}
