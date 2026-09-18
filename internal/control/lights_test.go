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

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// TestFloodlightModeAndStateAreNotTheSameThing pins the mapping
// docs/control.md's floodlight table records. Conflating mode and state
// makes the light look as though it only has an on switch: mode is what
// the light does, state is whether it is lit right now.
func TestFloodlightModeAndStateAreNotTheSameThing(t *testing.T) {
	cases := []struct {
		mode, state int
		want        string
	}{
		{0, 0, "off"},
		{1, 1, "on"},
		{1, 0, "motion"},
		{3, 0, "schedule"},
	}
	for _, tc := range cases {
		if got := floodlightState(tc.mode, tc.state); got != tc.want {
			t.Fatalf("mode %d state %d read as %q, want %q", tc.mode, tc.state, got, tc.want)
		}
	}
}

// loginReply is what a camera sends back for a successful CGI Login.
func loginReply(token string) string {
	return `[{"cmd":"Login","code":0,"value":{"Token":{"name":"` + token + `","leaseTime":3600}}}]`
}

// fakeCGICamera is a minimal CGI camera: it logs in and answers
// GetWhiteLed with whatever mode/state getWhiteLed currently holds, plus a
// fixed bright field this codebase never curates a control for, and
// records every SetWhiteLed it receives so a test can check what actually
// reached it, not just what the handler claims. The SetWhiteLed handler
// fails the test itself if bright arrives as anything but the value
// GetWhiteLed reported, the same discipline fakeClockCamera's SetTime
// handler already applies to "year" in applyall_test.go: an unmodelled
// field surviving the round trip is the thing under test, not a value this
// test reads back afterward.
type fakeCGICamera struct {
	srv   *httptest.Server
	mode  int32
	state int32
	sets  int32
}

// fakeCGICameraBright is the unmodelled field value newFakeCGICamera seeds
// GetWhiteLed with, and every SetWhiteLed must echo back unchanged.
const fakeCGICameraBright = 66

func newFakeCGICamera(t *testing.T, mode, state int) *fakeCGICamera {
	t.Helper()
	f := &fakeCGICamera{mode: int32(mode), state: int32(state)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "GetWhiteLed":
			mode := atomic.LoadInt32(&f.mode)
			state := atomic.LoadInt32(&f.state)
			w.Write([]byte(`[{"cmd":"GetWhiteLed","code":0,"value":{"WhiteLed":{"mode":` +
				strconv.Itoa(int(mode)) + `,"state":` + strconv.Itoa(int(state)) +
				`,"bright":` + strconv.Itoa(fakeCGICameraBright) + `}}}]`))
		case "SetWhiteLed":
			body, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					WhiteLed struct {
						Mode   int `json:"mode"`
						State  int `json:"state"`
						Bright int `json:"bright"`
					} `json:"WhiteLed"`
				} `json:"param"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad SetWhiteLed body: %v", err)
			} else if len(req) == 1 {
				if req[0].Param.WhiteLed.Bright != fakeCGICameraBright {
					t.Errorf("SetWhiteLed dropped the bright field it was never asked to change: got %d, want %d",
						req[0].Param.WhiteLed.Bright, fakeCGICameraBright)
				}
				atomic.StoreInt32(&f.mode, int32(req[0].Param.WhiteLed.Mode))
				atomic.StoreInt32(&f.state, int32(req[0].Param.WhiteLed.State))
			}
			atomic.AddInt32(&f.sets, 1)
			w.Write([]byte(`[{"cmd":"SetWhiteLed","code":0}]`))
		default:
			t.Errorf("unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCGICamera) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// TestServeApplyFloodlightWritesThroughCGIAndReachesTheCamera is the
// end-to-end proof for the floodlight's POST handler: choosing "on" must
// result in the fake camera's SetWhiteLed actually receiving mode 1, state
// 1, the pair docs/control.md's table maps to "on". The outcome tops out
// at accepted, never confirmed: the floodlight's read-back runs on the
// same session the write went out on, which can answer from state the
// camera has not committed, so this codebase has no right to claim more
// than the camera accepted the command. See serveApplyFloodlight.
func TestServeApplyFloodlightWritesThroughCGIAndReachesTheCamera(t *testing.T) {
	cam := newFakeCGICamera(t, 0, 0)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// The floodlight redirects back to the camera page with a one-line
	// banner rather than rendering a before/after page. The banner's own
	// wording still has to stop short of "confirmed": a same-session
	// read-back is not proof.
	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/cam1/floodlight", url.Values{"option": {"on"}})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303: %s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Location"); got != "/cameras/cam1" {
		t.Fatalf("redirected to %q, want the camera page the floodlight control lives on", got)
	}
	banner := takenFlash(t, s, resp)
	if banner.Outcome != "accepted" {
		t.Fatalf("banner reports %q, want accepted: %+v", banner.Outcome, banner)
	}

	// The actual proof: what the fake camera now holds, not what the
	// handler claims.
	if atomic.LoadInt32(&cam.mode) != 1 || atomic.LoadInt32(&cam.state) != 1 {
		t.Fatalf("camera holds mode=%d state=%d, want mode=1 state=1 (\"on\")", cam.mode, cam.state)
	}
	if atomic.LoadInt32(&cam.sets) != 1 {
		t.Fatalf("camera received %d SetWhiteLed calls, want 1", cam.sets)
	}
}

// TestServeApplyFloodlightRefusesAnUnknownOption proves a request naming
// something other than off/on/motion/schedule never reaches the camera at
// all: nothing here should even attempt to dial.
func TestServeApplyFloodlightRefusesAnUnknownOption(t *testing.T) {
	dialed := false
	cgiDial := func(c Camera) (*cgi.Client, error) {
		dialed = true
		return nil, nil
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.PostForm(ts.URL+"/cameras/cam1/floodlight", url.Values{"option": {"strobe"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	if dialed {
		t.Fatal("an unrecognised option must be refused before dialing the camera")
	}
}

// TestServeSettingsRendersLightsAndIRWithAConfirmReason proves the settings
// page carries the White LED field (message 209, curated in groups()), the
// floodlight section (CGI, seeded from the fake camera's current mode and
// state), and that both carry the confirm-every-time reason where a person
// reading the page can see it, not only in a comment.
func TestServeSettingsRendersLightsAndIRWithAConfirmReason(t *testing.T) {
	osdGetPair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
	getOsdPair, ok := pairForName("get osd")
	if !ok {
		t.Fatal("get osd pair not found")
	}
	ispPair, ok := pairForName("isp get")
	if !ok {
		t.Fatal("isp get pair not found")
	}
	ledPair, ok := pairForName("led get")
	if !ok {
		t.Fatal("led get pair not found")
	}
	ledXML := testXMLHeader + `<body><LedState><channelId>0</channelId><state>1</state></LedState></body>`

	// serveSettings reads every group's block on one connection, in order:
	// Camera name and overlay (its two OSD candidates), then Image, then
	// Lights and IR. fakecam streams its whole fixture the instant it
	// accepts, so a reply for a later group that arrives before its own
	// request is read gets discarded as a mismatched id by an earlier
	// group's read rather than saved for its turn. This test cares about
	// the Lights and IR group, but its fixture still has to answer every
	// read that precedes it, in that same order, or its own led reply
	// never survives to be read.
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, osdGetPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, getOsdPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, ispPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, ledPair.Get, 200, ledXML)...)

	baichuanCam := fakecam.New(t, fixture)
	cgiCam := newFakeCGICamera(t, 1, 0) // mode 1, state 0: "motion"

	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, baichuanCam.Addr(), baichuan.Options{Password: ""})
	}
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cgiCam.addr(), "admin", "")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial, CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/cameras/cam1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	for _, want := range []string{"Lights and IR", "Status LED", "Floodlight", lightsConfirmReason, irLivesInImageWarning} {
		if !strings.Contains(html, want) {
			t.Errorf("page does not render %q", want)
		}
	}
	// The Picture and OSD group from Task 7 must still be here: appending
	// the new group must not have dropped it.
	for _, want := range []string{"Camera name and overlay", "Camera name", "Image", "Brightness"} {
		if !strings.Contains(html, want) {
			t.Errorf("page no longer renders the Picture and OSD group's %q", want)
		}
	}
	// The floodlight option seeded as current ("motion", from mode 1 state
	// 0) must be selected, not left blank.
	if !strings.Contains(html, `value="motion" selected`) {
		t.Errorf("page does not show the floodlight's current state as selected:\n%s", html)
	}
	// Every form in this group must ask before it submits.
	if strings.Count(html, "onsubmit=\"return confirm(") < 2 {
		t.Errorf("not every Lights and IR / floodlight form confirms before submitting:\n%s", html)
	}
}

// TestCuratedFieldFindsTheWhiteLedField pins message 209's field into
// curatedField, the same check serveApplySetting uses to refuse a request
// naming anything it did not itself curate.
func TestCuratedFieldFindsTheWhiteLedField(t *testing.T) {
	if _, ok := curatedField("led get", "LedState/state"); !ok {
		t.Fatal("curatedField does not know the White LED field")
	}
}

// TestSetFloodlightPreservesEveryOtherField is the regression for the
// floodlight write composing a three-field document from nothing: a real
// WhiteLed object carries fields this codebase does not model, such as
// bright, and setFloodlight must echo them back unchanged rather than drop
// them. fakeCGICamera's SetWhiteLed handler fails the test itself if
// bright arrives as anything but what GetWhiteLed reported, the same
// discipline TestSetTimeZonePreservesEveryOtherField uses for "year" in
// applyall_test.go, so this is checked on the wire, not just against
// setFloodlight's return value.
func TestSetFloodlightPreservesEveryOtherField(t *testing.T) {
	cam := newFakeCGICamera(t, 0, 0)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), CGIDial: cgiDial})
	camera, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}

	c, err := s.cgiDial(camera)
	if err != nil {
		t.Fatal(err)
	}
	if err := setFloodlight(context.Background(), c, 1, 1); err != nil {
		t.Fatalf("setFloodlight: %v", err)
	}
	if atomic.LoadInt32(&cam.sets) != 1 {
		t.Fatalf("camera received %d SetWhiteLed calls, want 1", cam.sets)
	}
}
