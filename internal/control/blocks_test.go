package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

func TestConfidenceLabels(t *testing.T) {
	cases := []struct {
		name string
		set  uint32
		want Confidence
	}{
		// Confirmed to take effect, each changed and read back on a fresh
		// connection, then restored byte-identical.
		{"osd set", 45, Proven},
		{"led set", 209, Proven},
		{"email cfg set", 43, Proven},
		// Accepted with any field changed, answered 200 every time, and
		// re-read identical every time, on two different models.
		{"md set", 47, KnownInert},
		{"floodlight set", 288, KnownInert},
	}
	for _, tc := range cases {
		got := confidenceOf(baichuan.ConfigPair{Set: tc.set, Name: tc.name})
		if got != tc.want {
			t.Fatalf("%s (%d) labelled %q, want %q", tc.name, tc.set, got, tc.want)
		}
	}
}

func TestUnsafeToRewriteBeatsUnverified(t *testing.T) {
	// Re-applying an encoder configuration makes the camera reconfigure its
	// pipeline, which interrupts the stream. That is a stronger warning than
	// "nobody has tested this", so it wins.
	//
	// The names below must resolve through baichuan.ConfigMessages, which is
	// keyed by normalised block name ("enc", "isp"), not by the firmware's
	// set-message wording ("set enc", "isp set"). A test that only skips
	// unresolved names and never checks whether any resolved at all would
	// pass having asserted nothing; resolved tracks that so this test fails
	// loudly instead.
	resolved := 0
	// matched counts pairs actually checked below. resolved alone is not
	// enough: a name can resolve through ConfigMessages and still have no
	// counterpart in ConfigPairs, which would leave the inner loop's
	// t.Fatalf unreachable for that name while resolved still climbed. Both
	// counters must be nonzero or this test has asserted nothing.
	matched := 0
	for _, name := range []string{"set enc", "isp set", "enc", "isp"} {
		id := baichuan.ConfigMessages[name]
		if id == 0 {
			continue
		}
		resolved++
		// The pair's Set id is what confidenceOf and baichuan.UnsafeToRewrite
		// key on. A get-side name like "enc" or "isp" resolves through
		// ConfigMessages to its own (get) id, so exercise both directions:
		// call confidenceOf with that id in the Get slot and with the
		// matching set id, found via ConfigPairs, in the Set slot.
		for _, pair := range baichuan.ConfigPairs() {
			if pair.Get != id {
				continue
			}
			matched++
			if got := confidenceOf(pair); got != UnsafeToRewrite {
				t.Fatalf("%s (get %d, set %d) labelled %q", pair.Name, pair.Get, pair.Set, got)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no name in the test table resolved through ConfigMessages; this test asserted nothing")
	}
	if matched == 0 {
		t.Fatal("no resolved name had a matching pair in ConfigPairs; this test asserted nothing")
	}
}

// buildBlockFixture answers every name baichuan.ConfigNames knows: the
// first with a real body and 200, the second with 400, and everything else
// with 405. It mirrors buildProbeFixture in camera_test.go but keeps the
// body of the first reply, since readBlocks needs XML, not just a status.
func buildBlockFixture(t *testing.T) (fixture []byte, names []string) {
	t.Helper()
	names = baichuan.ConfigNames()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture = loginHandshake(testProbeNonce, testProbeDeviceInfo)
	for i, name := range names {
		id := baichuan.ConfigMessages[name]
		switch i {
		case 0:
			fixture = append(fixture, statusReply(t, key, id, 200, testXMLHeader+"<body><block>one</block></body>")...)
		case 1:
			fixture = append(fixture, statusReply(t, key, id, 400, "")...)
		default:
			fixture = append(fixture, statusReply(t, key, id, 405, "")...)
		}
	}
	return fixture, names
}

func TestReadBlocksCapturesXMLForA200AndNothingElse(t *testing.T) {
	fixture, names := buildBlockFixture(t)
	cam := fakecam.New(t, fixture)
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), Dial: dial})
	camObj, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := s.readBlocks(context.Background(), camObj)
	if err != nil {
		t.Fatalf("readBlocks: %v", err)
	}
	if len(blocks) != len(names) {
		t.Fatalf("got %d blocks, want %d", len(blocks), len(names))
	}
	if blocks[0].Status != 200 || !strings.Contains(blocks[0].XML, "<block>one</block>") {
		t.Fatalf("block 0 = %+v, want status 200 carrying the fixture body", blocks[0])
	}
	if blocks[1].Status != 400 || blocks[1].XML != "" {
		t.Fatalf("block 1 = %+v, want status 400 with no body", blocks[1])
	}
	for i := 2; i < len(blocks); i++ {
		if blocks[i].Status != 405 {
			t.Fatalf("block %d (%s) status %d, want 405", i, blocks[i].Name, blocks[i].Status)
		}
	}
}

// This is the enforcement of the rule that nothing on the page is editable
// that was not first read from the camera: buildPairRows must only seed and
// enable a pair whose matching read actually came back 200.
func TestBuildPairRowsOnlySeedsEditorsFromA200Read(t *testing.T) {
	blocks := []Block{
		{Name: "osd", ID: 29, Status: 200, XML: "<body><osd>1</osd></body>"},
		{Name: "led", ID: 208, Status: 400},
		{Name: "md", ID: 46, HungUp: true},
	}
	rows := buildPairRows(blocks)
	seen := map[uint32]PairRow{}
	for _, r := range rows {
		seen[r.Get] = r
	}
	if row, ok := seen[29]; !ok || !row.Editable || row.Seed != "<body><osd>1</osd></body>" {
		t.Fatalf("osd pair (get 29) = %+v, want an editable row seeded from the 200 read", row)
	}
	if row, ok := seen[208]; !ok || row.Editable || row.Seed != "" {
		t.Fatalf("led pair (get 208, status 400) = %+v, want no seed and Editable=false", row)
	}
	if row, ok := seen[46]; !ok || row.Editable || row.Seed != "" {
		t.Fatalf("md pair (get 46, hung up) = %+v, want no seed and Editable=false", row)
	}
}

func TestServeBlocksRendersXMLAndConfidenceLabels(t *testing.T) {
	fixture, _ := buildBlockFixture(t)
	cam := fakecam.New(t, fixture)
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/camera/cam1/blocks")
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

	// html/template escapes the XML for safe embedding in <pre>, so look for
	// the escaped form rather than the literal angle brackets.
	if !strings.Contains(html, "&lt;block&gt;one&lt;/block&gt;") {
		t.Error("page does not show the XML of the first (200) block")
	}
	for _, want := range []string{string(Proven), string(Unverified), string(KnownInert), string(UnsafeToRewrite)} {
		if !strings.Contains(html, want) {
			t.Errorf("page does not mention the %q label", want)
		}
	}
}

// buildBlockFixtureForPair answers every name baichuan.ConfigNames knows
// with 405, except pair's own Get id, which answers 200 carrying body. It
// is buildBlockFixture's targeted sibling, for a test that needs a
// specific writable pair's row to come back Editable rather than whichever
// pair happens to be first in ConfigNames order.
func buildBlockFixtureForPair(t *testing.T, pair baichuan.ConfigPair, body string) []byte {
	t.Helper()
	names := baichuan.ConfigNames()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	for _, name := range names {
		id := baichuan.ConfigMessages[name]
		if id == pair.Get {
			fixture = append(fixture, statusReply(t, key, id, 200, testXMLHeader+body)...)
		} else {
			fixture = append(fixture, statusReply(t, key, id, 405, "")...)
		}
	}
	return fixture
}

// TestBlocksPageEditorPostsToServeWrite is the regression for the raw
// block editor's textarea sitting in no form at all: the seeded editor
// must be wired to a form posting to serveWrite's own route
// (/camera/{name}/write/{id}), carrying the seeded document as its body
// field, so the raw view can actually promote a write from unverified to
// proven rather than only display one nothing on the page can submit.
func TestBlocksPageEditorPostsToServeWrite(t *testing.T) {
	pair := pickTestPair(t)
	body := "<body><field>seed-value</field></body>"
	fixture := buildBlockFixtureForPair(t, pair, body)
	cam := fakecam.New(t, fixture)
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/camera/cam1/blocks")
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

	wantAction := fmt.Sprintf(`action="/camera/cam1/write/%d"`, pair.Set)
	if !strings.Contains(html, wantAction) {
		t.Fatalf("page does not carry a form posting to %s:\n%s", wantAction, html)
	}
	if !strings.Contains(html, `name="body"`) {
		t.Fatalf("page carries no textarea named \"body\" for serveWrite to read:\n%s", html)
	}
	if !strings.Contains(html, "seed-value") {
		t.Fatalf("the editor is not seeded from the block this page just read:\n%s", html)
	}
}
