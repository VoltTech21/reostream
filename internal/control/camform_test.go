package control

import (
	"net/url"
	"os"
	"path/filepath"
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

// The form has no RTSP field, so an edit must not be read as "serve
// nothing over RTSP": that silently cut a camera off from go2rtc.
func TestFormEditKeepsRTSP(t *testing.T) {
	cfg := &config.Config{
		RTSP: &config.RTSPConfig{Listen: "0.0.0.0:8561"},
		Cameras: []config.Camera{
			{Name: "a", Address: "1.1.1.1", Streams: []string{"main", "sub"}, RTSP: []string{"main", "sub"}},
		},
	}
	form := url.Values{
		"name":     {"a"},
		"address":  {"1.1.1.2"},
		"username": {""},
		"password": {passwordUnchanged},
		"streams":  {"main", "sub", "extern"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Cameras[0].RTSP, ","); got != "main,sub,extern" {
		t.Fatalf("rtsp = %q, want every stream", got)
	}
}

func TestFormAddServesRTSPWhenItIsOn(t *testing.T) {
	cfg := &config.Config{RTSP: &config.RTSPConfig{Listen: "0.0.0.0:8561"}}
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
	if got := strings.Join(cfg.Cameras[0].RTSP, ","); got != "main,sub" {
		t.Fatalf("rtsp = %q", got)
	}
}

// The form has no channel field. Editing a hub camera from the page must
// keep its channel, or the next save points it at channel 0, another camera.
func TestFormEditKeepsAHubCamerasChannel(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "yard", Address: "1.1.1.1", Channel: 2, Password: "x", Streams: []string{"main"}},
	}}
	form := url.Values{
		"name":     {"yard"},
		"address":  {"1.1.1.1"},
		"password": {passwordUnchanged},
		"streams":  {"main", "sub"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].Channel != 2 {
		t.Fatalf("channel became %d", cfg.Cameras[0].Channel)
	}
	if !strings.Contains(cameraBlockText(cfg.Cameras[0]), "channel = 2\n") {
		t.Fatal("the written block drops the channel")
	}
	if strings.Contains(cameraBlockText(config.Camera{Name: "a"}), "channel") {
		t.Fatal("a standalone camera's block gained a channel line")
	}
}

// Camera settings address channel 0 on every path, which behind a hub is a
// different camera, so a hub camera's settings page must refuse outright.
func TestSettingsRefuseAHubCamera(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := "[control]\nlisten = \"127.0.0.1:0\"\npassword = \"abcdefghijklmnop\"\n\n" +
		"[[camera]]\nname = \"yard\"\naddress = \"192.0.2.9\"\nchannel = 1\nstreams = [\"main\"]\n\n" +
		"[[camera]]\nname = \"porch\"\naddress = \"192.0.2.10\"\nstreams = [\"main\"]\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{ConfigPath: path}}
	if _, err := s.byName("yard"); err == nil || !strings.Contains(err.Error(), "channel 1") {
		t.Fatalf("byName(hub camera) = %v, want a refusal naming the channel", err)
	}
	if _, err := s.byName("porch"); err != nil {
		t.Fatalf("byName(standalone camera) = %v", err)
	}
}

// The form has no ONVIF fields either. Editing a camera on the page must not
// quietly switch its PTZ endpoint off, and the written block must carry it.
func TestFormEditKeepsTheONVIFEndpoint(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "lelentos", Address: "1.1.1.1", Password: "x", Streams: []string{"main"},
			ONVIF: "127.0.0.1:8656", ONVIFUser: "frigate", ONVIFPassword: "$ONVIF_PW"},
	}}
	form := url.Values{
		"name":     {"lelentos"},
		"address":  {"1.1.1.1"},
		"password": {passwordUnchanged},
		"streams":  {"main", "sub"},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	block := cameraBlockText(cfg.Cameras[0])
	for _, want := range []string{"onvif = \"127.0.0.1:8656\"\n", "onvif_user = \"frigate\"\n", "onvif_password = \"$ONVIF_PW\"\n"} {
		if !strings.Contains(block, want) {
			t.Errorf("block lost %q:\n%s", want, block)
		}
	}
	if strings.Contains(cameraBlockText(config.Camera{Name: "a"}), "onvif") {
		t.Error("a camera with no PTZ gained ONVIF lines")
	}
}

func TestChangingAnONVIFEndpointNeedsARestart(t *testing.T) {
	a := &config.Config{Cameras: []config.Camera{{Name: "x", ONVIF: ":8656"}}}
	b := &config.Config{Cameras: []config.Camera{{Name: "x", ONVIF: ":8657"}}}
	same := &config.Config{Cameras: []config.Camera{{Name: "x", ONVIF: ":8656"}}}
	if got := listenersChanged(a, b); len(got) != 1 || got[0] != "onvif" {
		t.Errorf("moved port: %v", got)
	}
	if got := listenersChanged(a, same); len(got) != 0 {
		t.Errorf("unchanged: %v", got)
	}
	if got := listenersChanged(&config.Config{}, a); len(got) != 1 {
		t.Errorf("added: %v", got)
	}
}
