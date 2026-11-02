package control

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
)

// The Audio page's talk gain lands in the camera's block as talk_gain_db,
// keeping what the form has no field for (rotate), and a value off the
// 3 dB steps is refused.
func TestTalkGainIsSavedToTheConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n" +
		"[[camera]]\nname = \"one\"\naddress = \"192.0.2.10\"\nusername = \"admin\"\npassword = \"$CAM_PASS\"\nstreams = [\"main\"]\nrotate = 90\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAM_PASS", "secret")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	banner := takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+"/cameras/one/talkgain", url.Values{"db": {"-12"}}))
	if banner.Outcome != "confirmed" || !strings.Contains(banner.Message, "-12 dB") {
		t.Errorf("banner %+v", banner)
	}
	c, err := config.LoadRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	cam := c.Cameras[0]
	if cam.TalkGainDB != -12 || cam.Rotate == nil || *cam.Rotate != 90 || cam.Password != "$CAM_PASS" {
		t.Errorf("saved %+v, want talk_gain_db -12 with rotate 90 and the password reference kept", cam)
	}

	banner = takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+"/cameras/one/talkgain", url.Values{"db": {"5"}}))
	if banner.Outcome != "refused" {
		t.Errorf("5 dB: banner %+v, want refused", banner)
	}
	if c, _ := config.LoadRaw(path); c.Cameras[0].TalkGainDB != -12 {
		t.Error("a refused value was written")
	}
}

// The control is on the Audio page of a camera that can talk, showing the
// saved value.
func TestAudioPageShowsTalkGain(t *testing.T) {
	sup, err := baichuan.ParseSupport([]byte(`<body><Support><audioTalk>1</audioTalk></Support></body>`))
	if err != nil {
		t.Fatal(err)
	}
	if !sup.CanTalk() {
		t.Fatal("the support document does not say talk")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/one/settings/audio", nil), "camera.html",
		cameraPage{Title: "one", Camera: Camera{Name: "one", TalkGainDB: -9}, Cat: "audio", Support: &sup})
	body := w.Body.String()
	if !strings.Contains(body, "/cameras/one/talkgain") || !strings.Contains(body, `<option value="-9" selected>-9 dB</option>`) {
		t.Error("no talk gain control showing -9 dB")
	}
}
