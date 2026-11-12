package control

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// TestUpsertFindsABlockInACRLFFile is a file saved from the Config page,
// which a browser submits with CRLF line endings. The block scan used to
// fail to decode such a block, take it for some other camera, and append
// a second block with the same name, which the save then refused as a
// duplicate.
func TestUpsertFindsABlockInACRLFFile(t *testing.T) {
	text := strings.ReplaceAll(`listen = "0.0.0.0:8560"

[[camera]]
name = "one"
address = "192.0.2.50"
streams = ["main", "sub"]

[[camera]]
name = "two"
address = "192.0.2.51"
streams = ["main"]
`, "\n", "\r\n")
	got := upsertCameraBlock(text, config.Camera{Name: "one", Address: "192.0.2.50", Streams: []string{"main"}})

	if n := strings.Count(got, `name = "one"`); n != 1 {
		t.Fatalf("camera one appears %d times, want 1, so the block was appended rather than replaced:\n%q", n, got)
	}
	if strings.Contains(got, `"sub"`) {
		t.Errorf("camera one still pulls sub:\n%q", got)
	}
	if !strings.Contains(got, `name = "two"`) {
		t.Errorf("editing camera one destroyed camera two:\n%q", got)
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

// Removing a camera takes its block and the blank lines around it, and
// leaves everything else -- including other cameras and the file's own
// comments and settings -- exactly as it was.
func TestRemoveCameraBlockTakesOnlyTheNamedBlock(t *testing.T) {
	text := `# header comment

listen = "0.0.0.0:8560"

[[camera]]
name = "one"
address = "192.0.2.50"

[[camera]]
name = "two"
address = "192.0.2.51"
`
	got, ok := removeCameraBlock(text, "one")
	if !ok {
		t.Fatal("removeCameraBlock did not find camera one")
	}
	for _, want := range []string{"# header comment", `listen = "0.0.0.0:8560"`, `name = "two"`, `address = "192.0.2.51"`} {
		if !strings.Contains(got, want) {
			t.Errorf("removing camera one destroyed %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `name = "one"`) || strings.Contains(got, `192.0.2.50`) {
		t.Errorf("camera one is still present:\n%s", got)
	}
	if n := strings.Count(got, "[[camera]]"); n != 1 {
		t.Errorf("%d camera blocks, want 1:\n%s", n, got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("removal left a double gap:\n%s", got)
	}
}

// Removing a camera that is not there changes nothing and says so, rather
// than mangling the file.
func TestRemoveCameraBlockLeavesAnUnknownNameAlone(t *testing.T) {
	text := "[[camera]]\nname = \"one\"\naddress = \"192.0.2.50\"\n"
	got, ok := removeCameraBlock(text, "nope")
	if ok {
		t.Error("removeCameraBlock claimed to remove a camera that is not in the file")
	}
	if got != text {
		t.Errorf("the file was changed despite no match:\n%s", got)
	}
}

// Removing the only camera leaves no stray camera block behind.
func TestRemoveCameraBlockCanEmptyTheCameras(t *testing.T) {
	text := "listen = \"0.0.0.0:8560\"\n\n[[camera]]\nname = \"one\"\naddress = \"192.0.2.50\"\n"
	got, ok := removeCameraBlock(text, "one")
	if !ok {
		t.Fatal("removeCameraBlock did not find camera one")
	}
	if strings.Contains(got, "[[camera]]") {
		t.Errorf("a camera block survived removing the only camera:\n%s", got)
	}
	if !strings.Contains(got, `listen = "0.0.0.0:8560"`) {
		t.Errorf("removing the camera took the rest of the file with it:\n%s", got)
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
		{Name: "yard", Address: "1.1.1.1", Channel: 2, UID: "95270000TESTUID1", Password: "x", Streams: []string{"main"}},
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
	if cfg.Cameras[0].UID != "95270000TESTUID1" || !strings.Contains(cameraBlockText(cfg.Cameras[0]), "uid = \"95270000TESTUID1\"\n") {
		t.Fatal("an edit drops the uid")
	}
}

// Camera settings address channel 0 on every path, which behind a hub is a
// different camera, so a hub camera's settings page must refuse outright.
// A camera behind a hub or NVR has its own settings, reached through the
// recorder on its channel, but the whole-device pages (clock, ports) would
// change the recorder, so those still refuse it.
func TestHubCameraSettingsButNotDeviceSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := "[control]\nlisten = \"127.0.0.1:0\"\npassword = \"abcdefghijklmnop\"\n\n" +
		"[[camera]]\nname = \"yard\"\naddress = \"192.0.2.9\"\nchannel = 1\nstreams = [\"main\"]\n\n" +
		"[[camera]]\nname = \"porch\"\naddress = \"192.0.2.10\"\nstreams = [\"main\"]\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{ConfigPath: path}}
	if cam, err := s.byName("yard"); err != nil || cam.Channel != 1 {
		t.Fatalf("byName(hub camera) = %+v, %v; want the camera with channel 1", cam, err)
	}
	if _, err := s.byNameDirect("yard"); err == nil || !strings.Contains(err.Error(), "channel 1") {
		t.Fatalf("byNameDirect(hub camera) = %v, want a refusal naming the channel", err)
	}
	if _, err := s.byNameDirect("porch"); err != nil {
		t.Fatalf("byNameDirect(standalone camera) = %v", err)
	}
}

// The round trip a browser makes: render the camera list, submit a row's
// password box exactly as the page filled it in, and keep the password.
// Posting the sentinel straight to applyCameraForm hid that the page
// never sent it intact (html/template turned its NUL into U+FFFD).
func TestCameraListRoundTripKeepsThePassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n[[camera]]\nname = \"gate\"\naddress = \"192.0.2.10\"\nusername = \"admin\"\npassword = \"s3cret-pass\"\nstreams = [\"main\"]\n"), 0o600)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cameras", nil))
	m := regexp.MustCompile(`<input type="password" name="password" value="([^"]*)"`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no password box on the camera list (status %d): %.300s", rec.Code, rec.Body.String())
	}
	cfg, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"gate"}, "address": {"192.0.2.10"}, "username": {"admin"}, "password": {html.UnescapeString(m[1])}, "streams": {"main", "sub"}}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatalf("saving the row as the page sent it: %v", err)
	}
	if got := cfg.Cameras[0].Password; got != "s3cret-pass" {
		t.Fatalf("password = %q, want it kept", got)
	}
}

// The camera list offers only streams a camera has: zoom only where the
// camera serves one, nothing but main and sub on a battery camera, and a
// stream already set is always there to be turned off.
func TestCameraListOffersOnlyStreamsTheCameraHas(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "gate")})
	ctx := context.Background()
	if got := s.availableStreams(ctx, config.Camera{Name: "porch", UID: "95270000TESTUID1"}); strings.Join(got, ",") != "main,sub" {
		t.Errorf("battery camera offered %v", got)
	}
	if got := s.availableStreams(ctx, config.Camera{Name: "gate", Streams: []string{"main", "zoom"}}); !slices.Contains(got, "zoom") {
		t.Errorf("a configured zoom stream is not offered to turn off: %v", got)
	}
	if got := s.availableStreams(ctx, config.Camera{Name: "gate", Streams: []string{"main"}}); slices.Contains(got, "zoom") {
		t.Errorf("zoom offered on a camera that does not serve it: %v", got)
	}
}

// The form has no fields for these, so an edit must keep them: saving a
// battery camera used to drop its motion_topic.
func TestFormEditKeepsMotionTopicHoldPictureAndAlwaysOn(t *testing.T) {
	cfg := &config.Config{
		Events: &config.EventsConfig{MQTT: "127.0.0.1:1883"},
		Cameras: []config.Camera{
			{Name: "porch", Address: "1.1.1.1", UID: "95270000TESTUID1", Password: "x",
				MotionTopic: "reostream/argus/motion", HoldPicture: true, AlwaysOn: true},
		},
	}
	form := url.Values{"name": {"porch"}, "address": {"1.1.1.1"}, "uid": {"95270000TESTUID1"}, "password": {passwordUnchanged}}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	block := cameraBlockText(cfg.Cameras[0])
	for _, want := range []string{"motion_topic = \"reostream/argus/motion\"\n", "hold_picture = true\n", "always_on = true\n"} {
		if !strings.Contains(block, want) {
			t.Fatalf("block lost %q:\n%s", want, block)
		}
	}
}

// The page's camera form has no uid field and a battery camera ticks no
// streams, so saving one used to fail with "pick at least one stream".
func TestFormEditOfABatteryCameraKeepsItsUID(t *testing.T) {
	cfg := &config.Config{Cameras: []config.Camera{
		{Name: "porch", Address: "1.1.1.1", UID: "95270000TESTUID1", Password: "x"},
	}}
	form := url.Values{"name": {"porch"}, "address": {"1.1.1.1"}, "username": {"admin"}, "password": {passwordUnchanged}}
	if err := applyCameraForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.Cameras[0].UID != "95270000TESTUID1" {
		t.Fatalf("uid became %q", cfg.Cameras[0].UID)
	}
}
