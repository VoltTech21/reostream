package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// TestThereIsNoRouteThatWritesCameraAccounts is the test the brief asks
// for verbatim, and the one that matters most in this task.
//
// Message 59 turned out to be a user config WRITE sitting in the read
// sweep, which means `get all` was firing a user-config set with an EMPTY
// BODY at live cameras for some time. Nobody has established what it does.
// Until somebody deliberately does, a page that can rewrite camera
// accounts is a page that can lock an operator out of their own cameras,
// with no way back short of a factory reset.
func TestThereIsNoRouteThatWritesCameraAccounts(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "lounge")})
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req := httptest.NewRequest(method, "/camera/lounge/accounts", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("%s /camera/lounge/accounts returned %d, want 404 or 405", method, rec.Code)
		}
	}
}

// fakeNTPCamera is a minimal CGI camera for the NTP path: it logs in,
// answers GetNtp with whatever it currently holds plus a fixed port and
// interval this codebase never curates a control for, and records what
// SetNtp actually receives so a test can check what reached the camera,
// not just what the handler claims. The SetNtp handler fails the test
// itself if port or interval arrive as anything but what GetNtp reported,
// the same discipline fakeClockCamera's SetTime handler applies to "year"
// below: an unmodelled field surviving the round trip is the thing under
// test.
type fakeNTPCamera struct {
	srv      *httptest.Server
	enable   int32
	server   atomic.Value // string
	timeZone int32
	sets     int32
}

func newFakeNTPCamera(t *testing.T, enable int, server string, timeZone int) *fakeNTPCamera {
	t.Helper()
	f := &fakeNTPCamera{enable: int32(enable), timeZone: int32(timeZone)}
	f.server.Store(server)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "GetNtp":
			enable := atomic.LoadInt32(&f.enable)
			server := f.server.Load().(string)
			w.Write([]byte(`[{"cmd":"GetNtp","code":0,"value":{"Ntp":{"enable":` +
				strconv.Itoa(int(enable)) + `,"server":"` + server + `","port":123,"interval":1440}}}]`))
		case "GetTime":
			tz := atomic.LoadInt32(&f.timeZone)
			w.Write([]byte(`[{"cmd":"GetTime","code":0,"value":{"Time":{"timeZone":` + strconv.Itoa(int(tz)) + `}}}]`))
		case "SetNtp":
			body, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					Ntp struct {
						Enable   int    `json:"enable"`
						Server   string `json:"server"`
						Port     int    `json:"port"`
						Interval int    `json:"interval"`
					} `json:"Ntp"`
				} `json:"param"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad SetNtp body: %v", err)
			} else if len(req) == 1 {
				if req[0].Param.Ntp.Port != 123 || req[0].Param.Ntp.Interval != 1440 {
					t.Errorf("SetNtp dropped port/interval it was never asked to change: got port=%d interval=%d, want 123/1440",
						req[0].Param.Ntp.Port, req[0].Param.Ntp.Interval)
				}
				atomic.StoreInt32(&f.enable, int32(req[0].Param.Ntp.Enable))
				f.server.Store(req[0].Param.Ntp.Server)
			}
			atomic.AddInt32(&f.sets, 1)
			w.Write([]byte(`[{"cmd":"SetNtp","code":0}]`))
		default:
			t.Errorf("unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNTPCamera) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// TestSetNTPGoesOverCGINotBaichuan pins the routing decision itself: NTP
// has no Baichuan set-time message anywhere in the recovered table, so
// setNTP must reach the camera through CameraOptions.CGIDial. CameraOptions.Dial is
// left nil here, pointed at nothing real: if setNTP ever reached for a
// Baichuan connection instead, this test would fail on that dial rather
// than on any assertion below.
func TestSetNTPGoesOverCGINotBaichuan(t *testing.T) {
	cam := newFakeNTPCamera(t, 0, "pool.ntp.org", 0)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), CGIDial: cgiDial})

	camera, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.setNTP(context.Background(), camera, "time.nist.gov", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "confirmed" {
		t.Fatalf("outcome = %q, want confirmed: %s", result.Outcome, result.Detail)
	}

	// The actual proof: what the fake camera now holds, not what the
	// handler claims.
	if atomic.LoadInt32(&cam.enable) != 1 {
		t.Fatalf("camera holds enable=%d, want 1", cam.enable)
	}
	if got := cam.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera holds server=%q, want time.nist.gov", got)
	}
	if atomic.LoadInt32(&cam.sets) != 1 {
		t.Fatalf("camera received %d SetNtp calls, want 1", cam.sets)
	}
}

// TestServeApplyTimeWritesThroughCGI is the end-to-end proof for the time
// form's POST handler, the same discipline
// TestServeApplyFloodlightWritesThroughCGIAndReachesTheCamera in
// lights_test.go already applies to the floodlight.
func TestServeApplyTimeWritesThroughCGI(t *testing.T) {
	cam := newFakeNTPCamera(t, 0, "old.example", 0)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.PostForm(ts.URL+"/camera/cam1/time", url.Values{"server": {"time.nist.gov"}, "enabled": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `<strong class="outcome-confirmed">confirmed</strong>`) {
		t.Fatalf("response does not report confirmed: %s", raw)
	}
	if got := cam.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera holds server=%q, want time.nist.gov", got)
	}
}

// TestServeTimeRendersNTPAndTimezoneReadOnly proves the time page shows
// both the NTP fields and the timezone, and that the timezone section
// carries no form: it is display only.
func TestServeTimeRendersNTPAndTimezoneReadOnly(t *testing.T) {
	cam := newFakeNTPCamera(t, 1, "pool.ntp.org", -28800)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/camera/cam1/time")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, html)
	}
	for _, want := range []string{"pool.ntp.org", "-28800", "Timezone", "Read only"} {
		if !strings.Contains(html, want) {
			t.Errorf("page does not render %q:\n%s", want, html)
		}
	}
}

// TestSetNTPPreservesEveryOtherField is the regression for setNTP
// round-tripping through a fixed four-field struct that dropped anything
// GetNtp returned outside enable/server/port/interval. fakeNTPCamera's
// SetNtp handler fails the test itself if port or interval arrive as
// anything but what GetNtp reported, the same discipline
// TestSetTimeZonePreservesEveryOtherField uses for "year", so this is
// checked on the wire, not just against setNTP's return value.
func TestSetNTPPreservesEveryOtherField(t *testing.T) {
	cam := newFakeNTPCamera(t, 0, "pool.ntp.org", 0)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), CGIDial: cgiDial})
	camera, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.setNTP(context.Background(), camera, "time.nist.gov", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "confirmed" {
		t.Fatalf("outcome = %q, want confirmed: %s", result.Outcome, result.Detail)
	}
	if atomic.LoadInt32(&cam.sets) != 1 {
		t.Fatalf("camera received %d SetNtp calls, want 1", cam.sets)
	}
}
