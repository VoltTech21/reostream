package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// A TrackMix on channel 1 of an RLN8-410 has the recorder answer 400 to the
// guard read and to calibrate, so neither is offered or sent for a camera
// behind a recorder.
func TestAutoReturnAndCalibrateAreNotOfferedBehindARecorder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n" +
		"[[camera]]\nname = \"one\"\naddress = \"192.0.2.10\"\nchannel = 1\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path,
		Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			dials.Add(1)
			return nil, errors.New("no camera here")
		}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for _, route := range []string{"/cameras/one/ptz/guard", "/cameras/one/ptz/calibrate"} {
		banner := takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+route, url.Values{"enable": {"1"}}))
		if banner.Outcome != "refused" || !strings.Contains(banner.Message, "not available through a recorder") {
			t.Errorf("%s: banner %+v, want a refusal that names the recorder", route, banner)
		}
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("%d dials, want none", n)
	}
	if v := s.readGuard(context.Background(), Camera{Name: "one", Channel: 1}); !v.BehindRecorder {
		t.Error("the settings page would still show auto return and calibrate")
	}
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/one/settings/ptz", nil), "camera.html",
		cameraPage{Title: "one", Camera: Camera{Name: "one"}, Cat: "ptz", PTZ: ptzView{BehindRecorder: true}})
	if body := w.Body.String(); strings.Contains(body, "/ptz/guard") || strings.Contains(body, "/ptz/calibrate") {
		t.Error("the page still has the auto return or calibrate forms")
	}
}

// The fake TrackMix never answered auto return over Baichuan 331 and did
// take CGI SetPtzGuard {benable, timeout} (10-30, bug 2), so with HTTP on
// the write goes there and is read back there.
func TestAutoReturnIsWrittenOverHTTPWhenTheCameraHasIt(t *testing.T) {
	var mu sync.Mutex
	enable, timeout := 0, 60
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch cmd := r.URL.Query().Get("cmd"); cmd {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "Logout":
			w.Write([]byte(`[{"cmd":"Logout","code":0,"value":{"rspCode":200}}]`))
		case "GetPtzGuard":
			b, _ := json.Marshal([]any{map[string]any{"cmd": cmd, "code": 0, "value": map[string]any{
				"PtzGuard": map[string]any{"channel": 0, "benable": enable, "timeout": timeout, "bexistPos": 1}}}})
			w.Write(b)
		case "SetPtzGuard":
			raw, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					PtzGuard map[string]any `json:"PtzGuard"`
				} `json:"param"`
			}
			if err := json.Unmarshal(raw, &req); err != nil || len(req) == 0 {
				t.Errorf("SetPtzGuard body %s", raw)
				return
			}
			sent = req[0].Param.PtzGuard
			enable, timeout = int(sent["benable"].(float64)), int(sent["timeout"].(float64))
			w.Write([]byte(`[{"cmd":"SetPtzGuard","code":0,"value":{"rspCode":200}}]`))
		default:
			t.Errorf("unexpected cmd %q", cmd)
		}
	}))
	t.Cleanup(srv.Close)
	cam := fakecam.New(t, loginHandshake(testProbeNonce, testProbeDeviceInfo))
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one"),
		Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
		},
		CGIDial: func(Camera) (*cgi.Client, error) {
			return cgi.Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
		}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	banner := takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+"/cameras/one/ptz/guard", url.Values{"enable": {"1"}, "timeout": {"10"}}))
	if banner.Outcome != "confirmed" || !strings.Contains(banner.Message, "on, after 10 s") {
		t.Errorf("banner %+v, want confirmed on after 10 s", banner)
	}
	mu.Lock()
	defer mu.Unlock()
	if sent == nil || sent["benable"] != float64(1) || sent["timeout"] != float64(10) {
		t.Errorf("SetPtzGuard sent %v", sent)
	}
	if _, ok := sent["cmdStr"]; ok {
		t.Error("the auto return write moves the guard position too")
	}
}

// Without HTTP the Baichuan write is the only way, and a camera that does
// not answer it is said so in seconds, pointing at HTTP, not after the idle
// timeout.
func TestAutoReturnOverBaichuanFailsFastWhenTheCameraDoesNotAnswer(t *testing.T) {
	old := guardAnswerWait
	guardAnswerWait = 300 * time.Millisecond
	t.Cleanup(func() { guardAnswerWait = old })
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzGuard2, 200,
		testXMLHeader+`<body><PtzGuard version="1.1"><channelId>0</channelId><timeout>60</timeout><benable>0</benable><bvalid>1</bvalid></PtzGuard></body>`)...)
	cam := fakecam.New(t, fixture)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one"),
		Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
		},
		CGIDial: func(Camera) (*cgi.Client, error) { return nil, errors.New("connection refused") }})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	start := time.Now()
	banner := takenFlash(t, s, postForm(t, flashBrowser(t), ts.URL+"/cameras/one/ptz/guard", url.Values{"enable": {"1"}}))
	if banner.Outcome != "refused" || !strings.Contains(banner.Message, "no answer") || !strings.Contains(banner.Message, "HTTP") {
		t.Errorf("banner %+v, want a refusal saying it got no answer and to turn HTTP on", banner)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	if !strings.Contains(decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDPtzGuard, key), "<benable>1</benable>") {
		t.Error("the Baichuan write did not go out with benable 1")
	}
}
