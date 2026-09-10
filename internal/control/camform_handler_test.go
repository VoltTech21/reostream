package control_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
)

// TestSavingOneCameraDoesNotLeakAnUnrelatedCamerasResolvedPassword proves
// the correction to Task 11's brief: the camera form must read the config
// file with config.LoadRaw, not config.Load, because Load resolves a
// "$NAME" password into the real secret. Folding an unrelated edit through
// a resolving loader and re-encoding would write every camera's live
// credential into the config file in plaintext, not just the one the
// operator touched.
func TestSavingOneCameraDoesNotLeakAnUnrelatedCamerasResolvedPassword(t *testing.T) {
	t.Setenv("CAM_PW", "the-real-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	initial := `listen = "0.0.0.0:8560"

[[camera]]
name = "watched"
address = "192.0.2.50"
password = "$CAM_PW"
streams = ["main"]

[[camera]]
name = "unrelated"
address = "192.0.2.51"
streams = ["main"]
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      path,
	})

	// Edit only the unrelated camera; the "watched" camera's password
	// field is never part of this submission.
	form := url.Values{
		"name":    {"unrelated"},
		"address": {"192.0.2.99"},
		"streams": {"main"},
	}
	resp, err := http.PostForm(ts.URL+"/cameras", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	if !strings.Contains(text, "$CAM_PW") {
		t.Fatalf("the unresolved reference is gone; config now reads:\n%s", text)
	}
	if strings.Contains(text, "the-real-secret") {
		t.Fatalf("the resolved secret was written to disk; config now reads:\n%s", text)
	}
}
