package control_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
)

// writeTestConfig writes a reostream config naming the given cameras and
// returns its path. Addresses are from the documentation range, never a
// real one, so a test that accidentally dials cannot reach anything. This
// mirrors routes_test.go's own writeTestConfig: that one lives in package
// control, unreachable from here.
//
// The [control] section saying allow_no_password is what makes the install
// CLAIMED, which is the state these pages are reached in. A config with no
// [control] section at all leaves it unclaimed and sends every route to the
// claim screen; see claim.go.
func writeTestConfig(t *testing.T, names ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n")
	for i, name := range names {
		fmt.Fprintf(&b, "\n[[camera]]\nname = %q\naddress = \"192.0.2.%d\"\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n", name, i+10)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// getBody performs the request through the server and returns the body as
// a string, failing the test on a non-200.
func getBody(t *testing.T, s *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200: %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// The constants and helpers below (through buildProbeFixture and
// replaceReply) are package control_test's own copies of camprobe_test.go
// and blocks_test.go's fixture builders, needed for the same reason
// writeTestConfig is duplicated in this file: those packages' own copies
// are unexported and this file lives in a different test package
// (control_test, not control), so they are unreachable from here.

const testXMLHeader = `<?xml version="1.0" encoding="UTF-8"?>`
const testProbeNonce = "camerapage-test-nonce-0000000000"
const testProbeDeviceInfo = testXMLHeader + `<body><DeviceInfo><typeInfo>IPC</typeInfo></DeviceInfo></body>`

// buildMessage frames body under h, setting MsgLen from its length the same
// way baichuan's own Writer does.
func buildMessage(h baichuan.Header, body []byte) []byte {
	h.MsgLen = uint32(len(body))
	return append(h.Encode(), body...)
}

// loginHandshake builds the two messages a Conn reads during login: the
// nonce reply, then a login reply carrying deviceInfoXML.
func loginHandshake(nonce, deviceInfoXML string) []byte {
	plainNonce := testXMLHeader + `<body><Encryption version="1.1"><type>md5</type><nonce>` + nonce + `</nonce></Encryption></body>`
	nonceReply := buildMessage(baichuan.Header{
		MsgID: baichuan.MsgIDLogin, Class: baichuan.ClassModern20,
		EncByte: baichuan.NegotiateByte, DirByte: baichuan.DirReply,
	}, baichuan.BCCrypt(0, []byte(plainNonce)))
	loginReply := buildMessage(baichuan.Header{MsgID: baichuan.MsgIDLogin, Class: baichuan.ClassZero},
		baichuan.BCCrypt(0, []byte(deviceInfoXML)))
	return append(nonceReply, loginReply...)
}

// statusReply builds one AES-encrypted post-login reply carrying status.
func statusReply(t *testing.T, key []byte, id uint32, status int16, body string) []byte {
	t.Helper()
	enc, err := baichuan.AESEncrypt(key, []byte(body))
	if err != nil {
		t.Fatalf("encrypt reply %d: %v", id, err)
	}
	return buildMessage(baichuan.Header{
		MsgID:   id,
		Class:   baichuan.ClassZero,
		EncByte: byte(uint16(status) & 0xff),
		DirByte: byte(uint16(status) >> 8),
	}, enc)
}

// buildProbeFixture answers every name baichuan.ConfigNames knows: the
// first with 200, the second with 400, and everything else with 405. A
// full sweep like this is what keeps probeCamera fast in a test: an
// unanswered name costs a real 4 second readTimeout before probeCamera
// gives up on it and reconnects, and baichuan.ConfigNames lists around a
// hundred of them.
func buildProbeFixture(t *testing.T) (fixture []byte, names []string) {
	t.Helper()
	names = baichuan.ConfigNames()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture = loginHandshake(testProbeNonce, testProbeDeviceInfo)
	for i, name := range names {
		id := baichuan.ConfigMessages[name]
		switch i {
		case 0:
			fixture = append(fixture, statusReply(t, key, id, 200, testXMLHeader+"<body/>")...)
		case 1:
			fixture = append(fixture, statusReply(t, key, id, 400, "")...)
		default:
			fixture = append(fixture, statusReply(t, key, id, 405, "")...)
		}
	}
	return fixture, names
}

// replaceReply swaps out the encrypted body of the first reply for id in
// fixture, keeping every other message (and the surrounding message
// framing) untouched. It exists so buildProbeFixture's blanket 405 answer
// for one particular message can be overridden with real content, without
// hand-building the whole fixture a second time.
func replaceReply(t *testing.T, fixture []byte, id uint32, key []byte, newBody string) []byte {
	t.Helper()
	enc, err := baichuan.AESEncrypt(key, []byte(newBody))
	if err != nil {
		t.Fatalf("encrypt replacement body: %v", err)
	}
	off := 0
	for off < len(fixture) {
		h, hn, err := baichuan.DecodeHeader(fixture[off:])
		if err != nil {
			t.Fatalf("decode fixture at %d: %v", off, err)
		}
		end := off + hn + int(h.MsgLen)
		if h.MsgID == id {
			h.EncByte = byte(200)
			h.DirByte = 0
			replaced := buildMessage(h, enc)
			out := append([]byte{}, fixture[:off]...)
			out = append(out, replaced...)
			out = append(out, fixture[end:]...)
			return out
		}
		off = end
	}
	t.Fatalf("fixture has no reply for message %d", id)
	return nil
}

// cgiFloodlightCamera is control_test's own minimal CGI camera double: it
// logs in and answers GetWhiteLed with a fixed mode/state, which is all
// readFloodlight (lights.go) ever asks for on this page. It duplicates the
// shape of lights_test.go's own fakeCGICamera (package control, and so
// unreachable from here) rather than sharing it, for the same reason
// writeTestConfig and noCGIDial are duplicated above.
type cgiFloodlightCamera struct {
	srv *httptest.Server
}

func newCGIFloodlightCamera(t *testing.T, mode, state int) *cgiFloodlightCamera {
	t.Helper()
	c := &cgiFloodlightCamera{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(`[{"cmd":"Login","code":0,"value":{"Token":{"name":"tok","leaseTime":3600}}}]`))
		case "GetWhiteLed":
			w.Write([]byte(`[{"cmd":"GetWhiteLed","code":0,"value":{"WhiteLed":{"mode":` +
				strconv.Itoa(mode) + `,"state":` + strconv.Itoa(state) + `,"bright":50}}}]`))
		default:
			t.Errorf("cgiFloodlightCamera: unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *cgiFloodlightCamera) addr() string { return strings.TrimPrefix(c.srv.URL, "http://") }

// pairGetID finds the message id for a read's firmware description (e.g.
// "osd get"), the same lookup settings.go's own unexported pairForName
// does over baichuan.ConfigPairs, which is unreachable from this package.
// ConfigMessages is keyed by a normalised name ("osd2", not "osd get"), so
// that map is the wrong lookup here.
func pairGetID(t *testing.T, name string) uint32 {
	t.Helper()
	for _, p := range baichuan.ConfigPairs() {
		if p.Name == name {
			return p.Get
		}
	}
	t.Fatalf("no config pair named %q", name)
	return 0
}

// TestCameraPageShowsAllSixCombinedProperties is the regression test for
// the merged camera page (camerapage.go, camera.html): one camera, one
// page, carrying its stream state, its bitrate, a playable video tile, its
// curated settings, its floodlight control, and a link to its raw
// protocol blocks, all at once. A future edit that silently drops any one
// of them must fail here.
//
// Reaching the page's populated branches, rather than its empty ones,
// needs three doubles: a fakecam that actually answers the Baichuan
// probe sweep and the curated settings reads (Dial), a minimal CGI double
// that actually answers GetWhiteLed (CGIDial), and a Hubs source
// reporting a browser-playable H.264 codec on this camera's sub stream, so
// cameraGroupFor's Tile is playable rather than the dashboard's own
// zero-value "not playable" default.
func TestCameraPageShowsAllSixCombinedProperties(t *testing.T) {
	fixture, _ := buildProbeFixture(t)
	key := baichuan.AESKey(testProbeNonce, "")

	// Real per-field documents, shaped like the RLC-810A fixtures
	// settings_test.go already verifies groups()'s curated XPaths against
	// (testdata/livefixtures/osd2.xml, isp.xml, led.xml), not an invented
	// shape: an XPath this camera does not actually carry would leave the
	// field unavailable rather than seeded, exactly the failure this test
	// exists to catch.
	osdXML := testXMLHeader + `<body><OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable></OsdChannelName><OsdDatetime><channelId>0</channelId><enable>1</enable></OsdDatetime></body>`
	ispXML := testXMLHeader + `<body><VideoInput><channelId>0</channelId><bright>120</bright><contrast>110</contrast><saturation>100</saturation></VideoInput><InputAdvanceCfg><channelId>0</channelId><DayNight><mode>auto</mode><IrcutMode>ir</IrcutMode><Threshold>medium</Threshold></DayNight></InputAdvanceCfg></body>`
	ledXML := testXMLHeader + `<body><LedState><channelId>0</channelId><state>auto</state></LedState></body>`
	abilityXML := testXMLHeader + `<body><AbilityInfo version="1.1"><userName>admin</userName></AbilityInfo></body>`

	// Support/Abilities (the section above Settings) are not one of this
	// test's six pinned properties, but GetAbilities waits out its own
	// 40 second phase timeout if message 151 never arrives at all, since
	// fakecam never closes its write side after replaying a fixture; a
	// cheap reply here is what keeps this test from actually costing 40
	// real seconds over something it never asserts on.
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDAbilityInfo, 200, abilityXML)...)
	probeCam := fakecam.New(t, fixture)

	// The curated settings read (resolveGroupBlock, called once per group
	// in groups() order: Camera name and overlay, Image, Lights and IR)
	// runs on its own, later connection, and reads exactly these three
	// blocks, in this order. It cannot share probeCam's fixture: a Conn
	// reads a connection's bytes strictly forward and never rewinds, so
	// once the earlier support/probe phases (or a mismatched request
	// order) have scanned past a message looking for something else, it
	// is gone for the rest of that connection. A fresh, minimal,
	// correctly-ordered fixture on its own connection sidesteps that
	// entirely, the same way settings_test.go's own
	// TestServeSettingsRendersTheCuratedFieldsSeededFromTheCamera does.
	settingsFixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	settingsFixture = append(settingsFixture, statusReply(t, key, pairGetID(t, "osd get"), 200, osdXML)...)
	settingsFixture = append(settingsFixture, statusReply(t, key, pairGetID(t, "isp get"), 200, ispXML)...)
	settingsFixture = append(settingsFixture, statusReply(t, key, pairGetID(t, "led get"), 200, ledXML)...)
	settingsCam := fakecam.New(t, settingsFixture)

	// serveCamera dials three times, in this order: once for
	// Support/Abilities, once inside probeCamera for the message sweep,
	// and once for the curated settings read. The first two share
	// probeCam; the third gets settingsCam. See the comment above for why
	// the third cannot reuse the first two's connection or fixture.
	var calls int
	dial := func(ctx context.Context, c control.Camera) (*baichuan.Conn, error) {
		calls++
		addr := probeCam.Addr()
		if calls >= 3 {
			addr = settingsCam.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}

	cgiCam := newCGIFloodlightCamera(t, 1, 1) // mode 1, state 1: "on"
	cgiDial := func(c control.Camera) (*cgi.Client, error) {
		return cgi.Dial(cgiCam.addr(), "admin", "")
	}

	h := hub.New(4)
	h.SetKeyframe("H264", nil)
	hubs := server.StaticHubs(map[string]*hub.Hub{"one/sub": h})

	s := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Dial:            dial,
		CGIDial:         cgiDial,
		Hubs:            hubs,
		Status: fixedStatus{
			"one/main": {Connected: true, Streaming: true, FPS: 24.9, BitrateBps: 6.2e6},
		},
	})
	body := getBody(t, s, "/cameras/one")

	// 1 and 2: stream state and bitrate, off the dashboard's own stats row
	// for this camera (see dashboard_test.go's own bitrate/state tests).
	for _, want := range []string{"streaming", "24.9", "6.2"} {
		if !strings.Contains(body, want) {
			t.Errorf("camera page is missing its stream state: no %q", want)
		}
	}

	// 3: the playable video tile. This markup (the data-src container and
	// its play button) exists ONLY on cameravideo's playable branch;
	// dashboard.html's non-playable branch renders only the "No browser
	// playable stream" hint text, so this fails if the page falls back to
	// it, e.g. because Hubs went unwired or the tile stopped joining by
	// camera name.
	if !strings.Contains(body, `class="cam-video" data-src="/stream/one_sub.ts"`) {
		t.Errorf("camera page does not render the playable video tile:\n%s", body)
	}
	if !strings.Contains(body, "Play one") {
		t.Errorf("camera page does not render this tile's play button")
	}
	if strings.Contains(body, "No browser playable stream") {
		t.Errorf("camera page fell back to the non-playable branch even though a playable codec was reported")
	}

	// 4: curated settings, a concrete field label AND the value that came
	// from the fixture, not just the static "Settings" <h2> that renders
	// unconditionally regardless of whether fields are wired at all.
	if !strings.Contains(body, "Camera name") {
		t.Errorf("camera page does not render the Camera name field")
	}
	if !strings.Contains(body, `value="lounge"`) {
		t.Errorf("camera page does not show the camera name read from the camera:\n%s", body)
	}
	if !strings.Contains(body, "Brightness") {
		t.Errorf("camera page does not render the Brightness field")
	}
	if !strings.Contains(body, `value="120"`) {
		t.Errorf("camera page does not show the brightness read from the camera:\n%s", body)
	}
	if strings.Contains(body, "not available:") {
		t.Errorf("a curated field that resolved cleanly was rendered as unavailable:\n%s", body)
	}

	// 5: the floodlight, seeded from the fake CGI camera's real
	// GetWhiteLed answer (mode 1, state 1 = "on"), not merely the always
	// present "Floodlight" <h3>.
	if !strings.Contains(body, "Floodlight") {
		t.Errorf("camera page does not render the Floodlight section")
	}
	if !strings.Contains(body, `value="on" selected`) {
		t.Errorf("camera page does not show the floodlight's current state as selected:\n%s", body)
	}

	// 6: the advanced/raw-blocks link, which only renders inside the
	// {{if .Probes}} block once probeCamera has actually completed a
	// sweep, never when a camera never dialled leaves Probes nil.
	if !strings.Contains(body, `<a href="/cameras/one/advanced">every block, as XML</a>`) {
		t.Errorf("camera page does not link to the advanced/raw-blocks view:\n%s", body)
	}
}
