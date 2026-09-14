package control

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

func TestSetFieldReplacesOneElementAndLeavesTheRestByteIdentical(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<body><Osd><channelId>0</channelId><osdChannel><enable>1</enable><name>front</name></osdChannel></Osd></body>`)
	got, err := setField(doc, "Osd/osdChannel/name", "lounge")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("<name>lounge</name>")) {
		t.Fatalf("field not replaced:\n%s", got)
	}
	// Everything else must survive untouched, including the declaration and
	// fields this code has never heard of. The camera supplies its own
	// schema and echoing it back is what makes writes work on models nobody
	// here has seen.
	if !bytes.Contains(got, []byte(`<?xml version="1.0" encoding="UTF-8" ?>`)) {
		t.Fatal("the XML declaration was lost")
	}
	if !bytes.Contains(got, []byte("<channelId>0</channelId>")) {
		t.Fatal("an unrelated field was lost")
	}
	if !bytes.Contains(got, []byte("<enable>1</enable>")) {
		t.Fatal("a sibling field was lost")
	}
}

func TestSetFieldRefusesAnAbsentElementRatherThanInventingIt(t *testing.T) {
	doc := []byte("<body><Osd></Osd></body>")
	if _, err := setField(doc, "Osd/osdChannel/name", "x"); err == nil {
		t.Fatal("setField invented an element the camera never sent")
	}
}

func TestSetFieldMatchesRegardlessOfWrappingDepth(t *testing.T) {
	// The same path must find the leaf whether or not the document wraps it
	// in an outer element setField was never told about, since a handler
	// only ever knows the path relative to the field it cares about, not
	// every possible wrapper a camera might choose to send it inside.
	bare := []byte("<Osd><osdChannel><name>front</name></osdChannel></Osd>")
	got, err := setField(bare, "Osd/osdChannel/name", "lounge")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("<name>lounge</name>")) {
		t.Fatalf("field not replaced in unwrapped document:\n%s", got)
	}
}

func TestSetFieldEscapesTheValue(t *testing.T) {
	doc := []byte("<Osd><osdChannel><name>front</name></osdChannel></Osd>")
	got, err := setField(doc, "Osd/osdChannel/name", "A & B < C")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("<name>A & B < C</name>")) {
		t.Fatal("an unescaped value was written into the document, producing invalid XML")
	}
	if !bytes.Contains(got, []byte("&amp;")) || !bytes.Contains(got, []byte("&lt;")) {
		t.Fatalf("value was not escaped:\n%s", got)
	}
}

// TestImageGroupIsMarkedUnsafeToRewrite pins Group.Block to an id
// confidenceOf and baichuan.UnsafeToRewrite actually agree is unsafe, and
// pins that a warning field rides along with it, so a future edit that
// drops either the block or the warning fails here rather than only
// showing up as a missing warning on a page nobody re-reads by eye.
func TestImageGroupIsMarkedUnsafeToRewrite(t *testing.T) {
	found := false
	for _, g := range groups() {
		if !slices.Contains(g.Blocks, "isp get") {
			continue
		}
		found = true
		pair, ok := pairForName("isp get")
		if !ok {
			t.Fatalf("group %q names block %q, which is not a known read/write pair", g.Title, "isp get")
		}
		if !baichuan.UnsafeToRewrite(pair.Set) {
			t.Fatalf("group %q's block %q is not unsafe to rewrite, but this test assumed it was", g.Title, "isp get")
		}
		hasWarning := false
		for _, f := range g.Fields {
			if f.Kind == "warning" {
				hasWarning = true
			}
		}
		if !hasWarning {
			t.Fatalf("group %q edits an unsafe-to-rewrite block with no warning field", g.Title)
		}
	}
	if !found {
		t.Fatal("no group names the isp get block, so this test asserted nothing")
	}
}

func TestPairForNameFindsTheOsdPair(t *testing.T) {
	pair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("pairForName did not find the osd get pair")
	}
	if pair.Get != 44 || pair.Set != 45 {
		t.Fatalf("osd get pair is {Get:%d Set:%d}, want {Get:44 Set:45}", pair.Get, pair.Set)
	}
}

// TestSetFieldRefusesAnAmbiguousMatchRatherThanGuessing pins the fix for
// the sharper edge of suffix matching: a short path matches every ancestor
// chain ending in the same names, so a document carrying that pair more
// than once, one per channel here, must not let the first one win. A wrong
// pick here is a wrong write, not a refusal, which is the one failure this
// codebase cares most about never producing silently.
func TestSetFieldRefusesAnAmbiguousMatchRatherThanGuessing(t *testing.T) {
	doc := []byte(`<body>` +
		`<channel><Isp><bright>100</bright></Isp></channel>` +
		`<channel><Isp><bright>200</bright></Isp></channel>` +
		`</body>`)
	_, err := setField(doc, "Isp/bright", "150")
	if err == nil {
		t.Fatal("setField silently wrote to one of two matching elements instead of refusing")
	}
	if !strings.Contains(err.Error(), "2 elements match") {
		t.Fatalf("error does not explain the ambiguity: %v", err)
	}
}

// TestServeSettingsRendersTheCuratedFieldsSeededFromTheCamera proves the
// route actually works end to end: a request for /camera/{name}/settings
// reaches serveSettings, which reads the osd and isp blocks off a real
// (fake) connection and seeds the page with what they actually said, not
// with a blank form.
func TestServeSettingsRendersTheCuratedFieldsSeededFromTheCamera(t *testing.T) {
	osdPair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
	ispPair, ok := pairForName("isp get")
	if !ok {
		t.Fatal("isp get pair not found")
	}
	ledPair, ok := pairForName("led get")
	if !ok {
		t.Fatal("led get pair not found")
	}

	// Shaped exactly like testdata/livefixtures/osd2.xml and isp.xml, a real
	// RLC-810A's own replies, not the invented "Osd/osdChannel" shape this
	// test used to fabricate. See groups()'s own comment on why that
	// mattered: the earlier XPaths matched nothing this camera actually
	// sends.
	osdXML := testXMLHeader + `<body><OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable></OsdChannelName><OsdDatetime><channelId>0</channelId><enable>1</enable></OsdDatetime></body>`
	ispXML := testXMLHeader + `<body><VideoInput><channelId>0</channelId><bright>120</bright><contrast>110</contrast><saturation>100</saturation></VideoInput><InputAdvanceCfg><channelId>0</channelId><DayNight><mode>auto</mode><IrcutMode>ir</IrcutMode><Threshold>medium</Threshold></DayNight></InputAdvanceCfg></body>`
	ledXML := testXMLHeader + `<body><LedState><channelId>0</channelId><state>auto</state></LedState></body>`

	// serveSettings reads every group's block on one connection, in the
	// order groups() declares them, and fakecam streams its whole fixture
	// up front: a reply that arrives before its own request is read gets
	// discarded as a mismatched id by an earlier read, not saved for its
	// turn. So every group needs a reply here, in that same order, even
	// though this test's assertions only look at Camera name and Image.
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, osdPair.Get, 200, osdXML)...)
	fixture = append(fixture, statusReply(t, key, ispPair.Get, 200, ispXML)...)
	fixture = append(fixture, statusReply(t, key, ledPair.Get, 200, ledXML)...)

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
	html := string(raw)

	for _, want := range []string{"Camera name", "Show camera name", "Show timestamp", "Brightness", "Contrast", "Saturation", "Day and night mode", "Infrared cut filter", unsafeToRewriteWarning} {
		if !strings.Contains(html, want) {
			t.Errorf("page does not render the %q field", want)
		}
	}
	// Seeded from the read, not blank: the camera name the fixture answered
	// with must appear as the value of its control.
	if !strings.Contains(html, `value="lounge"`) {
		t.Errorf("page does not show the camera name read from the camera:\n%s", html)
	}
	if !strings.Contains(html, `value="120"`) {
		t.Errorf("page does not show the brightness read from the camera:\n%s", html)
	}
	if !strings.Contains(html, `value="ir"`) {
		t.Errorf("page does not show the infrared cut filter read from the camera:\n%s", html)
	}
	if strings.Contains(html, "not available:") {
		t.Errorf("a field that resolved cleanly was rendered as unavailable:\n%s", html)
	}
}

// TestSetFieldRefusesASelfClosingElementRatherThanSplicingAfterIt pins the
// self-closing edge case: <enable/> gives the decoder no separate open and
// close tags, so the offset where a normal element's text would sit is
// actually the offset right after the whole element, i.e. where a sibling
// would start. Splicing there would not set enable's content, it would
// invent a text node after it, silently producing a document nobody asked
// for. Every fixture elsewhere in this file uses explicit open/close pairs,
// so nothing else exercises this path.
func TestSetFieldRefusesASelfClosingElementRatherThanSplicingAfterIt(t *testing.T) {
	doc := []byte("<Osd><osdChannel><enable/></osdChannel></Osd>")
	before := append([]byte(nil), doc...)

	_, err := setField(doc, "Osd/osdChannel/enable", "1")
	if err == nil {
		t.Fatal("setField spliced text after a self-closing element instead of refusing it")
	}
	if !bytes.Equal(doc, before) {
		t.Fatal("setField must not touch its input even when it refuses")
	}
}

// buildApplySettingFixtures builds the two connections' worth of replies
// serveApplySetting's write needs: writeCam answers the manual pre-read
// (the handler's own step 2) and, on a second connection, writeBlock's own
// Before read plus the Set acknowledgement; verifyCam answers writeBlock's
// fresh verification read with the document as it stands after the write.
func buildApplySettingFixtures(t *testing.T, pair baichuan.ConfigPair, before, after string, setStatus int16) (writeCam, verifyCam *fakecam.Camera) {
	t.Helper()
	key := baichuan.AESKey(testProbeNonce, "")

	writeFixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	writeFixture = append(writeFixture, statusReply(t, key, pair.Get, 200, testXMLHeader+before)...)
	writeFixture = append(writeFixture, statusReply(t, key, pair.Set, setStatus, "")...)
	writeCam = fakecam.New(t, writeFixture)

	verifyFixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	verifyFixture = append(verifyFixture, statusReply(t, key, pair.Get, 200, testXMLHeader+after)...)
	verifyCam = fakecam.New(t, verifyFixture)
	return writeCam, verifyCam
}

// decodedSetConfigBody finds the last SetConfig message for id in received
// and decrypts its second section (the document itself; the first is the
// channel extension), so a test can check what actually went out on the
// wire rather than trusting that a 200 or a "confirmed" outcome implies it.
func decodedSetConfigBody(t *testing.T, received []byte, id uint32, key []byte) string {
	t.Helper()
	var found []byte
	off := 0
	for off+20 <= len(received) {
		h, n, err := baichuan.DecodeHeader(received[off:])
		if err != nil {
			break
		}
		end := off + n + int(h.MsgLen)
		if end > len(received) {
			break
		}
		if h.MsgID == id && h.MsgLen > h.PayloadOff {
			sealedBody := received[off+n+int(h.PayloadOff) : end]
			if dec, err := baichuan.AESDecrypt(key, sealedBody); err == nil {
				found = dec
			}
		}
		off = end
	}
	if found == nil {
		t.Fatalf("no two-section SetConfig message %d reached the camera", id)
	}
	return string(found)
}

// TestServeApplySettingWritesThroughWriteBlockAndReachesTheCamera is the
// end-to-end proof for the whole POST handler: a form submission for one
// curated field must result in the camera actually receiving a SetConfig
// document carrying the new value, not merely a 200 from the handler.
func TestServeApplySettingWritesThroughWriteBlockAndReachesTheCamera(t *testing.T) {
	pair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
	before := `<body><OsdChannelName><channelId>0</channelId><name>front</name><enable>1</enable></OsdChannelName></body>`
	after := `<body><OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable></OsdChannelName></body>`

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

	resp, err := http.PostForm(ts.URL+"/cameras/cam1/settings", url.Values{
		"block": {"osd get"},
		"xpath": {"OsdChannelName/name"},
		"value": {"lounge"},
	})
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

	// The actual proof: what reached the camera, not what the handler
	// claims. writeBlock sends the Set request on its own connection
	// (calls == 2 above), so it is writeCam that received it.
	key := baichuan.AESKey(testProbeNonce, "")
	sent := decodedSetConfigBody(t, writeCam.Received(), pair.Set, key)
	if !strings.Contains(sent, "<name>lounge</name>") {
		t.Fatalf("the document actually sent to the camera does not carry the edit: %s", sent)
	}
	if !strings.Contains(sent, "<channelId>0</channelId>") {
		t.Fatalf("the document actually sent to the camera lost an unrelated field: %s", sent)
	}
}

// TestServeApplySettingRefusesWhenTheFieldDoesNotResolve is the case the
// inferred image XPaths make realistic on real hardware: a curated field
// whose path does not match this camera's actual document. Nothing must be
// sent to the camera, and the operator must see a refusal, never a silent
// success.
func TestServeApplySettingRefusesWhenTheFieldDoesNotResolve(t *testing.T) {
	pair, ok := pairForName("isp get")
	if !ok {
		t.Fatal("isp get pair not found")
	}
	// A document this model actually returned, but with no
	// InputAdvanceCfg/DayNight/IrcutMode at all: the curated field names a
	// path this camera does not carry.
	before := `<body><VideoInput><channelId>0</channelId><bright>120</bright></VideoInput></body>`

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

	resp, err := http.PostForm(ts.URL+"/cameras/cam1/settings", url.Values{
		"block": {"isp get"},
		"xpath": {"InputAdvanceCfg/DayNight/IrcutMode"},
		"value": {"ir"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `<strong class="outcome-refused">refused</strong>`) {
		t.Fatalf("an unresolved field must be reported as refused, got: %s", raw)
	}
	if strings.Contains(string(raw), `<strong class="outcome-confirmed">confirmed</strong>`) || strings.Contains(string(raw), `<strong class="outcome-accepted">accepted</strong>`) {
		t.Fatalf("an unresolved field must never read as any kind of success: %s", raw)
	}
	// Nothing to write was ever composed, so nothing should have reached
	// the camera beyond the read this handler itself took.
	off := 0
	received := cam.Received()
	for off+20 <= len(received) {
		h, n, err := baichuan.DecodeHeader(received[off:])
		if err != nil {
			break
		}
		if h.MsgID == pair.Set {
			t.Fatal("a SetConfig message reached the camera for a field that never resolved")
		}
		off += n + int(h.MsgLen)
	}
}

// TestCuratedXPathsResolveAgainstRealLiveFixtures builds the fixes
// directly against testdata/livefixtures: a real RLC-810A's own osd2.xml,
// isp.xml and led.xml. An earlier version of groups() invented XPaths that
// matched none of these documents at all (Finding 4); this pins every
// currently curated, non-warning field to a path present in the actual
// capture, so a future invented path fails here rather than only showing
// up as a blank box on a real camera.
func TestCuratedXPathsResolveAgainstRealLiveFixtures(t *testing.T) {
	docs := map[string][]byte{
		"osd get": readTestdata(t, "testdata/livefixtures/osd2.xml"),
		"isp get": readTestdata(t, "testdata/livefixtures/isp.xml"),
		"led get": readTestdata(t, "testdata/livefixtures/led.xml"),
	}
	want := map[string]string{
		"OsdChannelName/name":                "Rear Bay",
		"OsdChannelName/enable":              "0",
		"OsdDatetime/enable":                 "1",
		"VideoInput/bright":                  "128",
		"VideoInput/contrast":                "128",
		"VideoInput/saturation":              "128",
		"InputAdvanceCfg/DayNight/mode":      "auto",
		"InputAdvanceCfg/DayNight/IrcutMode": "ir",
		"LedState/state":                     "auto",
	}
	for _, g := range groups() {
		for _, f := range g.Fields {
			if f.Kind == "warning" {
				continue
			}
			var doc []byte
			for _, blockName := range g.Blocks {
				if d, ok := docs[blockName]; ok {
					doc = d
					break
				}
			}
			if doc == nil {
				t.Fatalf("field %q's group %q names no block this test has a fixture for (%v)", f.XPath, g.Title, g.Blocks)
			}
			got, ok := fieldValue(doc, f.XPath)
			if !ok {
				t.Errorf("%s does not resolve against the real fixture for group %q", f.XPath, g.Title)
				continue
			}
			if w, ok := want[f.XPath]; ok && got != w {
				t.Errorf("%s = %q, want %q from the live fixture", f.XPath, got, w)
			}
		}
	}
}

// readTestdata reads a testdata file relative to this package's directory,
// failing the test if it is missing.
func readTestdata(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// TestNoDefaultsOnlyBlockCanEverBeCuratedTogether pins the structural
// safety property groups()'s own comment on Blocks relies on: a
// defaults-only read such as "osd def get" (110, testdata/livefixtures/
// osd3.xml, the FACTORY DEFAULTS for the exact same settings osd2.xml
// carries live) never appears in ConfigPairs at all, because it has no
// matching "osd def set". Had the curated OSD group ever fallen back onto
// it, writing back through it would have renamed the camera to "Camera1"
// and switched its OSD language to Chinese. This test is the guarantee
// that resolveGroupBlock has no path to it, not just that groups() does
// not currently list it.
func TestNoDefaultsOnlyBlockCanEverBeCuratedTogether(t *testing.T) {
	for _, defaultsName := range []string{"osd def get", "isp def"} {
		if _, ok := pairForName(defaultsName); ok {
			t.Fatalf("%q resolved to a writable pair; a defaults-only read must never pair with a Set", defaultsName)
		}
	}
}

// TestResolveGroupBlockFallsBackToTheSecondCandidate is Finding 4's
// discovery mechanism: a camera that answers 405 for the first candidate
// but 200 for the second must still seed the group, from whichever
// candidate actually worked, not fail the whole group because the first
// guess was wrong. This is exactly the shop_rear-probe.txt fact this page
// was verified against: "osd" (get osd, 29) answers 405 there while "osd2"
// (osd get, 44) answers 200.
func TestResolveGroupBlockFallsBackToTheSecondCandidate(t *testing.T) {
	firstName, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
	secondName, ok := pairForName("get osd")
	if !ok {
		t.Fatal("get osd pair not found")
	}
	osdXML := testXMLHeader + `<body><OsdChannelName><channelId>0</channelId><name>Rear Bay</name><enable>0</enable></OsdChannelName></body>`

	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, firstName.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, secondName.Get, 200, osdXML)...)
	cam := fakecam.New(t, fixture)

	conn, err := baichuan.Dial(context.Background(), cam.Addr(), baichuan.Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	g := Group{Title: "Camera name and overlay", Blocks: []string{"osd get", "get osd"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pair, doc, reason := resolveGroupBlock(ctx, conn, g)
	if reason != "" {
		t.Fatalf("resolveGroupBlock did not fall back to the working candidate: %s", reason)
	}
	if pair.Name != "get osd" {
		t.Fatalf("resolved block = %q, want %q", pair.Name, "get osd")
	}
	if v, ok := fieldValue(doc, "OsdChannelName/name"); !ok || v != "Rear Bay" {
		t.Fatalf("resolved document did not seed the expected value: %q, ok=%v", v, ok)
	}
}

// TestUnavailableFieldRendersExplanationNotBlankInput is Finding 3: a
// curated field whose block never answered must say so where the control
// would be, not render an empty, silently-broken editable input.
func TestUnavailableFieldRendersExplanationNotBlankInput(t *testing.T) {
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

	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	// Every group's read gets an explicit, fast 405, in request order: both
	// OSD candidates, then Image, then Lights. This camera implements
	// none of them.
	fixture = append(fixture, statusReply(t, key, osdGetPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, getOsdPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, ispPair.Get, baichuan.StatusNotImplemented, "")...)
	fixture = append(fixture, statusReply(t, key, ledPair.Get, baichuan.StatusNotImplemented, "")...)
	cam := fakecam.New(t, fixture)

	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	cgiCam := newFakeCGICamera(t, 0, 0)
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	if !strings.Contains(html, "not available:") {
		t.Fatalf("page does not explain the unresolved OSD group:\n%s", html)
	}
	if strings.Contains(html, `name="xpath" value="OsdChannelName/name"`) {
		t.Fatalf("page rendered an editable input for a field whose block never answered:\n%s", html)
	}
}
