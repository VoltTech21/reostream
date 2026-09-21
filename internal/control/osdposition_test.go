package control

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// The overlay position control: where the timestamp and the camera name
// sit. What the corner values mean, and how that was established off real
// hardware, is settings.go's corner table; these tests pin the two halves
// of the control built on it -- that the corner shown is the camera's own,
// and that choosing one reaches the camera as a single write carrying both
// of the elements a corner is made of.

// mustPair is pairForName with the not-found case turned into a test
// failure, so a fixture builder reads as one line per block.
func mustPair(t *testing.T, name string) baichuan.ConfigPair {
	t.Helper()
	p, ok := pairForName(name)
	if !ok {
		t.Fatalf("no config pair named %q", name)
	}
	return p
}

// renderCameraPageWithOSD renders /cameras/cam1 against a fake camera whose
// "osd get" block answers with osdXML, and returns the HTML.
//
// The Image and Lights groups get minimal, real-shaped replies too, in the
// order groups() asks for them: baichuan.Conn reads a connection strictly
// forward and never rewinds, so a fixture that answers out of order costs a
// real timeout rather than an assertion.
func renderCameraPageWithOSD(t *testing.T, osdBody string) string {
	t.Helper()
	key := baichuan.AESKey(testProbeNonce, "")
	ispXML := testXMLHeader + `<body><VideoInput><channelId>0</channelId><bright>128</bright><contrast>128</contrast><saturation>128</saturation></VideoInput><InputAdvanceCfg><channelId>0</channelId><DayNight><mode>auto</mode><IrcutMode>ir</IrcutMode></DayNight></InputAdvanceCfg></body>`
	ledXML := testXMLHeader + `<body><LedState><channelId>0</channelId><state>auto</state></LedState></body>`

	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, mustPair(t, "osd get").Get, 200, testXMLHeader+osdBody)...)
	fixture = append(fixture, statusReply(t, key, mustPair(t, "isp get").Get, 200, ispXML)...)
	fixture = append(fixture, statusReply(t, key, mustPair(t, "led get").Get, 200, ledXML)...)
	cam := fakecam.New(t, fixture)

	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial, CGIDial: noCGIDial})
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
	return string(raw)
}

// TestPositionSelectIsSeededFromTheCamerasOwnPair is the read half of the
// control: the corner shown as current must be the one this camera's own
// topLeftX/topLeftY say it is, never a default. The fixture puts the
// timestamp top right (65536,1) and the camera name bottom right
// (65536,65536), the exact combination one real camera on the fleet this
// table came from was holding.
func TestPositionSelectIsSeededFromTheCamerasOwnPair(t *testing.T) {
	// Parallel with the other page-rendering test below, and with nothing
	// else: rendering the camera page waits out the support and probe
	// phases against a fixture that never answers them, which is most of a
	// minute of wall clock and no CPU at all. Nothing here is shared --
	// each test builds its own fake camera, its own config file and its
	// own server.
	t.Parallel()
	html := renderCameraPageWithOSD(t, `<body>`+
		`<OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable><topLeftX>65536</topLeftX><topLeftY>65536</topLeftY></OsdChannelName>`+
		`<OsdDatetime><channelId>0</channelId><enable>1</enable><topLeftX>65536</topLeftX><topLeftY>1</topLeftY></OsdDatetime>`+
		`</body>`)

	for _, want := range []string{"Camera name position", "Timestamp position"} {
		if !strings.Contains(html, want) {
			t.Errorf("page does not render the %q control", want)
		}
	}
	// Both elements of one position travel as one xpath, which is what
	// lets the handler change them inside a single read-modify-write.
	for _, want := range []string{
		`name="xpath" value="OsdDatetime/topLeftX,OsdDatetime/topLeftY"`,
		`name="xpath" value="OsdChannelName/topLeftX,OsdChannelName/topLeftY"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("a position form does not name both of its elements: no %s\n%s", want, html)
		}
	}
	// Seeded, not defaulted: the two overlays in this fixture sit in two
	// DIFFERENT corners, so a page that always selected the same one, or
	// simply the first option, fails here.
	if !strings.Contains(html, `<option value="65536,1" selected>top right</option>`) {
		t.Errorf("the timestamp's own corner (65536,1 = top right) is not selected:\n%s", html)
	}
	if !strings.Contains(html, `<option value="65536,65536" selected>bottom right</option>`) {
		t.Errorf("the camera name's own corner (65536,65536 = bottom right) is not selected:\n%s", html)
	}
	// All four corners are offered, or this is a display, not a control.
	for _, want := range []string{"top left", "top right", "bottom left", "bottom right"} {
		if !strings.Contains(html, ">"+want+"</option>") {
			t.Errorf("the position select does not offer %q", want)
		}
	}
	if strings.Contains(html, "not a known corner") {
		t.Errorf("a pair straight out of the corner table was reported as unrecognised:\n%s", html)
	}
	if strings.Contains(html, "not available:") {
		t.Errorf("a position that resolved cleanly was rendered as unavailable:\n%s", html)
	}
}

// TestUnrecognisedPositionIsShownNotSnapped is the honesty case. Only 1 and
// 65536 were ever observed on real hardware, so any other pair is a
// position this project cannot name -- and naming it anyway, by rounding it
// to the nearest corner, would silently discard a position somebody chose,
// at the very moment they opened the page to look at it. It must be shown
// as itself, selected, with the four corners still one click away.
func TestUnrecognisedPositionIsShownNotSnapped(t *testing.T) {
	t.Parallel() // see the note on the test above
	html := renderCameraPageWithOSD(t, `<body>`+
		`<OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable><topLeftX>65536</topLeftX><topLeftY>65536</topLeftY></OsdChannelName>`+
		`<OsdDatetime><channelId>0</channelId><enable>1</enable><topLeftX>32768</topLeftX><topLeftY>9</topLeftY></OsdDatetime>`+
		`</body>`)

	if !strings.Contains(html, `<option value="32768,9" selected>32768,9 (not a known corner)</option>`) {
		t.Fatalf("an unrecognised position was not shown as the camera's own pair:\n%s", html)
	}
	// Snapping would have selected a corner instead. The timestamp's pair
	// is unrecognised so none of the corners may be selected for it; the
	// camera name above it genuinely is bottom right, and still is.
	for _, corner := range []string{"1,1", "65536,1", "1,65536"} {
		if strings.Contains(html, `<option value="`+corner+`" selected>`) {
			t.Errorf("an unrecognised position was snapped to the %s corner:\n%s", corner, html)
		}
	}
	if !strings.Contains(html, "not one of the four") {
		t.Errorf("the page does not say why this position has no name:\n%s", html)
	}
	if !strings.Contains(html, `<option value="1,1" >top left</option>`) {
		t.Errorf("the four corners are not still offered alongside the unrecognised pair:\n%s", html)
	}
}

// countTwoSectionWrites counts the two-section SetConfig messages for id
// that actually reached the camera. Two sections is the shape a real write
// takes (the channel extension, then the document); anything narrower is
// not a write of a document at all.
func countTwoSectionWrites(received []byte, id uint32) int {
	n, off := 0, 0
	for off+20 <= len(received) {
		h, hn, err := baichuan.DecodeHeader(received[off:])
		if err != nil {
			break
		}
		end := off + hn + int(h.MsgLen)
		if end > len(received) {
			break
		}
		if h.MsgID == id && h.MsgLen > h.PayloadOff {
			n++
		}
		off = end
	}
	return n
}

// TestPositionWriteCarriesBothFieldsInOneWrite is the whole point of the
// handler's generalisation. A corner is topLeftX AND topLeftY, and two
// separate read-modify-write round trips would mean a camera that accepted
// the first and refused the second left the overlay somewhere nobody chose.
// So this asserts what reached the CAMERA, not what the page says: exactly
// one SetConfig for this block, carrying both changed elements and
// everything else the document was read with.
func TestPositionWriteCarriesBothFieldsInOneWrite(t *testing.T) {
	pair := mustPair(t, "osd get")
	// Top left now; the write moves it to the bottom right, which changes
	// BOTH elements, so a write carrying only one of them is visibly wrong
	// rather than coincidentally right.
	before := `<body><OsdDatetime><channelId>0</channelId><enable>1</enable><topLeftX>1</topLeftX><topLeftY>1</topLeftY></OsdDatetime></body>`
	after := `<body><OsdDatetime><channelId>0</channelId><enable>1</enable><topLeftX>65536</topLeftX><topLeftY>65536</topLeftY></OsdDatetime></body>`

	writeCam, verifyCam := buildApplySettingFixtures(t, pair, before, after, 200)
	calls := 0
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		calls++
		addr := writeCam.Addr()
		if calls == 3 {
			addr = verifyCam.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/cam1/settings", url.Values{
		"block": {"osd get"},
		"xpath": {"OsdDatetime/topLeftX,OsdDatetime/topLeftY"},
		"value": {"65536,65536"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303: %s", resp.StatusCode, raw)
	}

	if n := countTwoSectionWrites(writeCam.Received(), pair.Set); n != 1 {
		t.Fatalf("the camera received %d writes for one position change, want exactly 1", n)
	}
	key := baichuan.AESKey(testProbeNonce, "")
	sent := decodedSetConfigBody(t, writeCam.Received(), pair.Set, key)
	if !strings.Contains(sent, "<topLeftX>65536</topLeftX>") {
		t.Errorf("the document sent to the camera does not carry the new X: %s", sent)
	}
	if !strings.Contains(sent, "<topLeftY>65536</topLeftY>") {
		t.Errorf("the document sent to the camera does not carry the new Y: %s", sent)
	}
	// The camera's own document with two fields changed, never one
	// composed here: everything it was read with must still be in it.
	if !strings.Contains(sent, "<channelId>0</channelId>") || !strings.Contains(sent, "<enable>1</enable>") {
		t.Errorf("the document sent to the camera lost an unrelated field: %s", sent)
	}
}

// TestAHalfUnappliablePositionWritesNothingAtAll states the atomicity claim
// as a wire fact. If one of a position's two elements cannot be applied to
// the document the camera just sent, the other must not be written on its
// own: the camera receives nothing.
func TestAHalfUnappliablePositionWritesNothingAtAll(t *testing.T) {
	pair := mustPair(t, "osd get")
	// A real document, but carrying no topLeftY at all: the second half of
	// the change has nowhere to land.
	before := `<body><OsdDatetime><channelId>0</channelId><enable>1</enable><topLeftX>1</topLeftX></OsdDatetime></body>`

	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, pair.Get, 200, testXMLHeader+before)...)
	cam := fakecam.New(t, fixture)

	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/cam1/settings", url.Values{
		"block": {"osd get"},
		"xpath": {"OsdDatetime/topLeftX,OsdDatetime/topLeftY"},
		"value": {"65536,65536"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303: %s", resp.StatusCode, raw)
	}
	if n := countTwoSectionWrites(cam.Received(), pair.Set); n != 0 {
		t.Fatalf("%d writes reached the camera for a change only half of which could be applied", n)
	}
}

// TestSingleFieldWriteStillParsesAsExactlyOneUnchangedEdit pins that
// generalising the handler to several fields did not change what a
// one-field form does. The end-to-end proof that a single write still
// reaches a camera is settings_test.go's
// TestServeApplySettingWritesThroughWriteBlockAndReachesTheCamera, which
// posts the same plain block/xpath/value it always did; this is the parsing
// rule underneath it, including the case a list format could plausibly have
// broken -- a value that itself contains a comma. The path count governs
// the split, so a camera named "Bay, Rear" is never cut in half.
func TestSingleFieldWriteStillParsesAsExactlyOneUnchangedEdit(t *testing.T) {
	edits, err := parseEdits("OsdChannelName/name", "Bay, Rear")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(edits, []edit{{XPath: "OsdChannelName/name", Value: "Bay, Rear"}}) {
		t.Fatalf("a one-field form did not parse as its own single unchanged edit: %#v", edits)
	}

	// A two-path form pairs by position.
	edits, err = parseEdits("OsdDatetime/topLeftX,OsdDatetime/topLeftY", "1,65536")
	if err != nil {
		t.Fatal(err)
	}
	want := []edit{
		{XPath: "OsdDatetime/topLeftX", Value: "1"},
		{XPath: "OsdDatetime/topLeftY", Value: "65536"},
	}
	if !slices.Equal(edits, want) {
		t.Fatalf("two-field form parsed as %#v, want %#v", edits, want)
	}

	// A value list without one part per path is refused, not padded: half
	// a position is a corner nobody chose.
	if _, err := parseEdits("OsdDatetime/topLeftX,OsdDatetime/topLeftY", "1"); err == nil {
		t.Fatal("a position missing half its value was accepted")
	}
}

// TestBothHalvesOfAPositionAreCurated pins the validation rule the write
// path depends on: a position's topLeftY is as curated as its topLeftX, so
// a form naming it passes the same check, and nothing outside groups() ever
// does.
func TestBothHalvesOfAPositionAreCurated(t *testing.T) {
	for _, xpath := range []string{
		"OsdDatetime/topLeftX", "OsdDatetime/topLeftY",
		"OsdChannelName/topLeftX", "OsdChannelName/topLeftY",
	} {
		f, ok := curatedField("osd get", xpath)
		if !ok {
			t.Errorf("%s is not curated, so a position write naming it would be refused", xpath)
			continue
		}
		if f.Kind != "position" {
			t.Errorf("%s resolved to a %q field, want a position", xpath, f.Kind)
		}
	}
	if _, ok := curatedField("osd get", "OsdDatetime/width"); ok {
		t.Error("a field groups() never declared resolved as curated")
	}
}
