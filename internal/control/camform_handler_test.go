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

[control]
listen = "0.0.0.0:8562"
allow_no_password = true

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
	resp, err := http.PostForm(ts.URL+"/cameras/add", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Two ways a test on this route goes quietly vacuous, and both leave
	// the file untouched with every "must not have destroyed" assertion
	// still passing: the route is /cameras/add, so a POST to /cameras
	// answers 405, and a config with no [control] section leaves the
	// install unclaimed, so every POST is answered with the claim screen
	// -- at 200. Hence the status check here, the [control] section in the
	// config above, and at least one assertion per test for something the
	// save must have ADDED.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /cameras/add: got %d, want 200", resp.StatusCode)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	if !strings.Contains(text, `address = "192.0.2.99"`) {
		t.Fatalf("the edit never reached the file; config now reads:\n%s", text)
	}
	if !strings.Contains(text, "$CAM_PW") {
		t.Fatalf("the unresolved reference is gone; config now reads:\n%s", text)
	}
	if strings.Contains(text, "the-real-secret") {
		t.Fatalf("the resolved secret was written to disk; config now reads:\n%s", text)
	}
}

// headerConfig is a config file that looks like one a claim wrote and a
// person then edited: a comment header carrying the -listen warning, keys
// capitalised the way whoever typed them capitalised them, and one camera
// already in it.
const headerConfig = `# reostream config.
#
# The listen addresses are the ones this daemon was already running on. A
# -listen flag on the command line still overrides the first one; if you
# pass one, change the line below to match it.

Listen = "0.0.0.0:8560"

[control]
Listen = "0.0.0.0:8562"
allow_no_password = true

# the one in the hallway
[[camera]]
Name = "one"
Address = "192.0.2.50"
streams = ["main"]
`

// TestAddingACameraKeepsTheFilesCommentsAndCasing is the defect: saving a
// camera used to re-encode the whole decoded config, which produced a file
// with every comment gone and every key capitalised by the encoder rather
// than by the person who wrote it.
//
// The comment header is not decoration. It carries the warning about
// -listen overriding the listen line, which a ledger ruling relied on when
// it parked a related finding, so destroying it silently retires that
// ruling's reasoning.
func TestAddingACameraKeepsTheFilesCommentsAndCasing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(headerConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: path})
	resp, err := http.PostForm(ts.URL+"/cameras/add", url.Values{
		"name":    {"two"},
		"address": {"192.0.2.51"},
		"streams": {"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /cameras/add: got %d, want 200", resp.StatusCode)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	for _, want := range []string{
		"# reostream config.",
		"-listen flag on the command line still overrides",
		"# the one in the hallway",
		`Listen = "0.0.0.0:8560"`,
		`Listen = "0.0.0.0:8562"`,
		`Name = "one"`,
		`Address = "192.0.2.50"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saving a camera destroyed %q; config now reads:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `name = "two"`) {
		t.Errorf("the new camera is not in the file:\n%s", text)
	}
	if !strings.Contains(text, `address = "192.0.2.51"`) {
		t.Errorf("the new camera has no address:\n%s", text)
	}
}

// TestAddingACameraLeavesAnotherCamerasPasswordReferenceAlone is the same
// guard the LoadRaw test above keeps, aimed at the other path: adding a
// camera rather than editing one. A reference must still be a reference
// afterwards, and the secret behind it must never reach the file.
func TestAddingACameraLeavesAnotherCamerasPasswordReferenceAlone(t *testing.T) {
	t.Setenv("CAM_PW", "the-real-secret")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	initial := `listen = "0.0.0.0:8560"

[control]
listen = "0.0.0.0:8562"
allow_no_password = true

[[camera]]
name = "watched"
address = "192.0.2.50"
password = "$CAM_PW"
streams = ["main"]
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: path})
	resp, err := http.PostForm(ts.URL+"/cameras/add", url.Values{
		"name":    {"added"},
		"address": {"192.0.2.51"},
		"streams": {"main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /cameras/add: got %d, want 200", resp.StatusCode)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, `password = "$CAM_PW"`) {
		t.Fatalf("the reference did not round-trip as a reference:\n%s", text)
	}
	if strings.Contains(text, "the-real-secret") {
		t.Fatalf("the resolved secret was written to disk:\n%s", text)
	}
	if !strings.Contains(text, `name = "added"`) {
		t.Fatalf("the new camera is not in the file:\n%s", text)
	}
}
