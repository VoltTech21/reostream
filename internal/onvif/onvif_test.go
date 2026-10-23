package onvif

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

type fakePTZ struct {
	mu      sync.Mutex
	calls   []string
	moving  bool
	presets []baichuan.PTZPreset
	err     error
}

func (f *fakePTZ) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakePTZ) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePTZ) Move(_ context.Context, cmd baichuan.PTZCommand, speed int) error {
	f.record(fmt.Sprintf("move %s %d", cmd, speed))
	f.mu.Lock()
	f.moving = true
	f.mu.Unlock()
	return f.err
}

func (f *fakePTZ) Stop(context.Context) error {
	f.record("stop")
	f.mu.Lock()
	f.moving = false
	f.mu.Unlock()
	return f.err
}

func (f *fakePTZ) Presets(context.Context) ([]baichuan.PTZPreset, error) {
	return f.presets, f.err
}

func (f *fakePTZ) GotoPreset(_ context.Context, id int) error {
	f.record(fmt.Sprintf("goto %d", id))
	return f.err
}

func (f *fakePTZ) Moving() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.moving
}

func soap(header, body string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" ` +
		`xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema" ` +
		`xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl">` +
		`<s:Header>` + header + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
}

func post(t *testing.T, h http.Handler, header, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://cam.local:8656/onvif/ptz_service", strings.NewReader(soap(header, body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func newHandler(p PTZ) *Handler {
	return NewHandler(p, Options{Name: "lelentos", Width: 2560, Height: 1440})
}

func TestCapabilitiesPointBackAtTheRequestHost(t *testing.T) {
	_, out := post(t, newHandler(&fakePTZ{}), "", `<tds:GetCapabilities/>`)
	for _, want := range []string{
		"http://cam.local:8656/onvif/device_service",
		"http://cam.local:8656/onvif/media_service",
		"http://cam.local:8656/onvif/ptz_service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

// Frigate picks the first profile that has a video encoder and a PTZ
// configuration with a continuous pan/tilt space, and offers no PTZ at all
// when there is none.
func TestTheProfileIsUsableForPTZ(t *testing.T) {
	_, out := post(t, newHandler(&fakePTZ{}), "", `<trt:GetProfiles/>`)
	for _, want := range []string{
		`<trt:Profiles token="profile_main" fixed="true">`,
		`<tt:VideoEncoderConfiguration`,
		`<tt:PTZConfiguration token="ptz_config">`,
		`<tt:DefaultContinuousPanTiltVelocitySpace>`,
		`<tt:Width>2560</tt:Width>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	// No zoom or relative spaces: a client that sees them will use them.
	for _, bad := range []string{"Zoom", "Relative", "Absolute"} {
		if strings.Contains(out, bad) {
			t.Errorf("profile advertises %s: %s", bad, out)
		}
	}
}

func TestContinuousMoveBecomesAStraightCommand(t *testing.T) {
	cases := []struct {
		x, y float64
		want string
	}{
		{-0.5, 0, "move left 32"},
		{0.5, 0, "move right 32"},
		{0, 0.5, "move up 32"},
		{0, -1, "move down 64"},
		// The camera takes no diagonals; the stronger axis wins.
		{-0.2, 0.9, "move up 58"},
		{0.8, -0.8, "move right 51"},
		// Tiny velocities still move.
		{0.001, 0, "move right 1"},
	}
	for _, c := range cases {
		p := &fakePTZ{}
		code, out := post(t, newHandler(p), "", fmt.Sprintf(
			`<tptz:ContinuousMove><tptz:ProfileToken>profile_main</tptz:ProfileToken>`+
				`<tptz:Velocity><tt:PanTilt x="%g" y="%g"/></tptz:Velocity></tptz:ContinuousMove>`, c.x, c.y))
		if code != 200 || !strings.Contains(out, "ContinuousMoveResponse") {
			t.Fatalf("(%g,%g): %d %s", c.x, c.y, code, out)
		}
		if got := p.log(); len(got) != 1 || got[0] != c.want {
			t.Errorf("(%g,%g): calls = %v, want %q", c.x, c.y, got, c.want)
		}
	}
}

func TestAZeroVelocityIsAStop(t *testing.T) {
	p := &fakePTZ{}
	post(t, newHandler(p), "", `<tptz:ContinuousMove><tptz:Velocity><tt:PanTilt x="0" y="0"/></tptz:Velocity></tptz:ContinuousMove>`)
	if got := p.log(); len(got) != 1 || got[0] != "stop" {
		t.Errorf("calls = %v", got)
	}
}

func TestAMoveTimeoutStopsTheCamera(t *testing.T) {
	p := &fakePTZ{}
	post(t, newHandler(p), "", `<tptz:ContinuousMove><tptz:Velocity><tt:PanTilt x="0.5" y="0"/></tptz:Velocity><tptz:Timeout>PT0.05S</tptz:Timeout></tptz:ContinuousMove>`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(p.log()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.log(); len(got) != 2 || got[1] != "stop" {
		t.Errorf("calls = %v, want a stop after the timeout", got)
	}
}

func TestStopAndStatus(t *testing.T) {
	p := &fakePTZ{}
	h := newHandler(p)
	post(t, h, "", `<tptz:ContinuousMove><tptz:Velocity><tt:PanTilt x="1" y="0"/></tptz:Velocity></tptz:ContinuousMove>`)
	_, out := post(t, h, "", `<tptz:GetStatus><tptz:ProfileToken>profile_main</tptz:ProfileToken></tptz:GetStatus>`)
	if !strings.Contains(out, "<tt:PanTilt>MOVING</tt:PanTilt>") {
		t.Errorf("status while moving: %s", out)
	}
	post(t, h, "", `<tptz:Stop><tptz:ProfileToken>profile_main</tptz:ProfileToken><tptz:PanTilt>true</tptz:PanTilt></tptz:Stop>`)
	_, out = post(t, h, "", `<tptz:GetStatus/>`)
	if !strings.Contains(out, "<tt:PanTilt>IDLE</tt:PanTilt>") {
		t.Errorf("status after stop: %s", out)
	}
}

func TestPresets(t *testing.T) {
	p := &fakePTZ{presets: []baichuan.PTZPreset{{ID: 0, Name: "0"}, {ID: 3, Name: "Door & porch"}, {ID: 7}}}
	h := newHandler(p)
	_, out := post(t, h, "", `<tptz:GetPresets><tptz:ProfileToken>profile_main</tptz:ProfileToken></tptz:GetPresets>`)
	for _, want := range []string{
		`<tptz:Preset token="0"><tt:Name>0</tt:Name>`,
		`<tptz:Preset token="3"><tt:Name>Door &amp; porch</tt:Name>`,
		`<tptz:Preset token="7"><tt:Name>preset 7</tt:Name>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	code, _ := post(t, h, "", `<tptz:GotoPreset><tptz:ProfileToken>profile_main</tptz:ProfileToken><tptz:PresetToken>3</tptz:PresetToken></tptz:GotoPreset>`)
	if got := p.log(); code != 200 || len(got) != 1 || got[0] != "goto 3" {
		t.Errorf("code %d calls %v", code, got)
	}
	code, out = post(t, h, "", `<tptz:GotoPreset><tptz:PresetToken>nope</tptz:PresetToken></tptz:GotoPreset>`)
	if code != http.StatusBadRequest || !strings.Contains(out, "Fault") {
		t.Errorf("bad token: %d %s", code, out)
	}
}

func TestAnUnsupportedOperationIsAFaultNotAnEmptyOK(t *testing.T) {
	for _, op := range []string{"RelativeMove", "AbsoluteMove", "SetPreset", "GetStreamUri", "Nonsense"} {
		code, out := post(t, newHandler(&fakePTZ{}), "", `<tptz:`+op+`/>`)
		if code == 200 || !strings.Contains(out, "ActionNotSupported") {
			t.Errorf("%s: %d %s", op, code, out)
		}
	}
}

func TestCameraFailuresReachTheClient(t *testing.T) {
	p := &fakePTZ{err: fmt.Errorf("camera refused")}
	code, out := post(t, newHandler(p), "", `<tptz:ContinuousMove><tptz:Velocity><tt:PanTilt x="1" y="0"/></tptz:Velocity></tptz:ContinuousMove>`)
	if code != http.StatusInternalServerError || !strings.Contains(out, "camera refused") {
		t.Errorf("%d %s", code, out)
	}
}

func TestMalformedRequests(t *testing.T) {
	h := newHandler(&fakePTZ{})
	for _, body := range []string{"", "not xml", "<a><b/></a>"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func token(user, pass string, nonce []byte, created time.Time, digest bool) string {
	c := created.UTC().Format(time.RFC3339)
	pw, typ := pass, "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText"
	if digest {
		s := sha1.New()
		s.Write(nonce)
		s.Write([]byte(c))
		s.Write([]byte(pass))
		pw = base64.StdEncoding.EncodeToString(s.Sum(nil))
		typ = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
	}
	return fmt.Sprintf(`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" `+
		`xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"><wsse:UsernameToken>`+
		`<wsse:Username>%s</wsse:Username><wsse:Password Type=%q>%s</wsse:Password>`+
		`<wsse:Nonce>%s</wsse:Nonce><wsu:Created>%s</wsu:Created></wsse:UsernameToken></wsse:Security>`,
		user, typ, pw, base64.StdEncoding.EncodeToString(nonce), c)
}

func TestAuthentication(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	h := NewHandler(&fakePTZ{}, Options{Name: "x", Username: "frigate", Password: "s3cret", Now: func() time.Time { return now }})
	nonce := []byte("0123456789abcdef")
	stop := `<tptz:Stop/>`
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"none", "", 401},
		{"digest ok", token("frigate", "s3cret", nonce, now, true), 200},
		{"digest wrong password", token("frigate", "nope", nonce, now, true), 401},
		{"wrong user", token("other", "s3cret", nonce, now, true), 401},
		{"digest replayed from yesterday", token("frigate", "s3cret", nonce, now.Add(-24*time.Hour), true), 401},
		{"text ok", token("frigate", "s3cret", nonce, now, false), 200},
		{"text wrong", token("frigate", "nope", nonce, now, false), 401},
	}
	for _, c := range cases {
		if code, _ := post(t, h, c.header, stop); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
	// A client needs the time to build a digest before it can authenticate.
	if code, _ := post(t, h, "", `<tds:GetSystemDateAndTime/>`); code != 200 {
		t.Errorf("GetSystemDateAndTime without a token: %d", code)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"PT5S": 5 * time.Second, "PT1M30S": 90 * time.Second, "PT0.5S": 500 * time.Millisecond, "PT2M": 2 * time.Minute,
	} {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("%q = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "5", "PT", "P1D", "PT-5S"} {
		if _, ok := parseDuration(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
}
