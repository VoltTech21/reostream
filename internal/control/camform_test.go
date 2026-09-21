package control

import (
	"net/url"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

func TestFormKeepsAnExistingPasswordWhenTheFieldIsLeftAlone(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "a", Address: "1.1.1.1", Username: "admin", Password: "secret", Streams: []string{"main"}},
	}}
	form := url.Values{
		"name":     {"a"},
		"address":  {"1.1.1.1"},
		"username": {"admin"},
		"password": {passwordUnchanged},
		"streams":  {"main"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].Password != "secret" {
		t.Fatalf("password became %q; the form must not be able to blank a password it never showed", cfg.Cameras[0].Password)
	}
}

func TestFormSetsANewPassword(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "a", Address: "1.1.1.1", Password: "secret", Streams: []string{"main"}},
	}}
	form := url.Values{
		"name":     {"a"},
		"address":  {"1.1.1.1"},
		"username": {""},
		"password": {"$CAM_PW"},
		"streams":  {"main"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].Password != "$CAM_PW" {
		t.Fatalf("password is %q", cfg.Cameras[0].Password)
	}
}

// TestUpsertReplacesOnlyTheNamedBlock is the edit path of the same rule
// the add path keeps: the file is the existing document with one thing
// changed, so a comment above another camera, another camera's own keys,
// and the header at the top all survive an edit to one block.
func TestUpsertReplacesOnlyTheNamedBlock(t *testing.T) {
	text := `# header comment

Listen = "0.0.0.0:8560"

# the one in the hallway
[[camera]]
Name = "one"
Address = "192.0.2.50"

[[camera]]
name = "two"
address = "192.0.2.51"
`
	got := upsertCameraBlock(text, config.Camera{Name: "two", Address: "192.0.2.99", Streams: []string{"main"}})

	for _, want := range []string{"# header comment", `Listen = "0.0.0.0:8560"`, "# the one in the hallway", `Name = "one"`, `Address = "192.0.2.50"`} {
		if !strings.Contains(got, want) {
			t.Errorf("editing camera two destroyed %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `address = "192.0.2.99"`) {
		t.Errorf("camera two was not edited:\n%s", got)
	}
	if strings.Contains(got, `address = "192.0.2.51"`) {
		t.Errorf("camera two's old address is still there, so the block was added rather than replaced:\n%s", got)
	}
	if n := strings.Count(got, "[[camera]]"); n != 2 {
		t.Errorf("%d camera blocks, want 2:\n%s", n, got)
	}
}

// TestUpsertIsNotFooledByAHeaderInsideAMultiLineString guards the scan
// that finds block boundaries. A TOML multi-line string can hold a line
// that reads as a table header, and a scanner that believed it would cut
// a block in the wrong place and rewrite text that was never a block.
func TestUpsertIsNotFooledByAHeaderInsideAMultiLineString(t *testing.T) {
	text := `[[camera]]
name = "one"
address = """192.0.2.50
[[camera]]
not a header
"""

[[camera]]
name = "two"
address = "192.0.2.51"
`
	got := upsertCameraBlock(text, config.Camera{Name: "two", Address: "192.0.2.99"})
	if !strings.Contains(got, "not a header") {
		t.Errorf("the multi-line string was cut up:\n%s", got)
	}
	if !strings.Contains(got, `address = "192.0.2.99"`) {
		t.Errorf("camera two was not edited:\n%s", got)
	}
}

// TestUpsertAppendsToAnEmptyFile is the first camera on a fresh install,
// where there is no text to preserve and nothing to separate the block
// from.
func TestUpsertAppendsToAnEmptyFile(t *testing.T) {
	got := upsertCameraBlock("", config.Camera{Name: "one", Address: "192.0.2.50"})
	if !strings.HasPrefix(got, "[[camera]]\n") {
		t.Fatalf("got %q, want a bare block", got)
	}
}

func TestFormAddsANewCamera(t *testing.T) {
	cfg := &config.Config{}
	form := url.Values{
		"name":     {"new"},
		"address":  {"192.0.2.9"},
		"username": {"admin"},
		"password": {""},
		"streams":  {"main", "sub"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cameras) != 1 || len(cfg.Cameras[0].Streams) != 2 {
		t.Fatalf("got %+v", cfg.Cameras)
	}
}

func TestFormRejectsAStreamTheDaemonDoesNotKnow(t *testing.T) {
	cfg := &config.Config{}
	form := url.Values{
		"name":    {"new"},
		"address": {"192.0.2.9"},
		"streams": {"ultra"},
	}
	if err := applyCameraForm(cfg, form); err == nil {
		t.Fatal("an unknown stream name was accepted")
	}
}
