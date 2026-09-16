package control

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// flashBrowser is a client that behaves like the browser these handlers
// were designed against: it keeps cookies (the flash is keyed by one) and
// it does NOT follow redirects, so a test can see the 303 itself rather
// than only whatever page it landed on.
func flashBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// getPage fetches one page with the browser's cookies and returns its body.
func getPage(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200: %s", url, resp.StatusCode, raw)
	}
	return string(raw)
}

// postForm posts with the browser's cookies, leaving any redirect
// unfollowed for the caller to inspect.
func postForm(t *testing.T, c *http.Client, url string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// takenFlash reads, and consumes, the banner a write left behind for the
// browser that made it. It lets a test check what a write reported without
// rendering the page it redirected to, which for the camera page means
// four more dials at a fake camera that has no fixtures left for them.
func takenFlash(t *testing.T, s *Server, resp *http.Response) Flash {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, ck := range resp.Cookies() {
		r.AddCookie(ck)
	}
	f := s.takeFlash(r)
	if f == nil {
		t.Fatal("the write left no banner behind")
	}
	return *f
}

// osdWriteServer wires up the fake cameras one curated OSD write needs and
// returns the running server's URL plus the camera that receives the
// SetConfig, so a test can check what actually went out on the wire.
//
// Three dials happen per curated write: the handler's own fresh pre-read,
// writeBlock's Before read plus the Set, and writeBlock's fresh
// verification read. The first two land on writeCam, the third on
// verifyCam, exactly as TestServeApplySettingWritesThroughWriteBlockAndReachesTheCamera
// already arranges them.
func osdWriteServer(t *testing.T, before, after string) (baseURL string, pair baichuan.ConfigPair, writeCam *fakecam.Camera) {
	t.Helper()
	pair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
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
	return ts.URL, pair, writeCam
}

const osdBefore = `<body><OsdChannelName><channelId>0</channelId><name>front</name><enable>1</enable></OsdChannelName></body>`
const osdAfter = `<body><OsdChannelName><channelId>0</channelId><name>lounge</name><enable>1</enable></OsdChannelName></body>`

// TestACuratedWriteRedirectsRatherThanRenderingAResultPage is the whole
// point of this change: a write must land the operator back on the page
// the setting lives on, so that a refresh is a plain GET and not a second
// write to the camera. The full before/after page it used to render had no
// way back at all.
func TestACuratedWriteRedirectsRatherThanRenderingAResultPage(t *testing.T) {
	base, _, _ := osdWriteServer(t, osdBefore, osdAfter)
	c := flashBrowser(t)

	resp := postForm(t, c, base+"/cameras/cam1/settings", url.Values{
		"block": {"osd get"},
		"xpath": {"OsdChannelName/name"},
		"value": {"lounge"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: a curated write must redirect, not render", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/cameras/cam1" {
		t.Fatalf("redirected to %q, want the camera page the setting lives on", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing of result.html's shape may come back on this response: the
	// whole complaint was the huge popup.
	if strings.Contains(string(raw), "restore previous") || strings.Contains(string(raw), "<pre") {
		t.Fatalf("the redirect still carries the old result page:\n%s", raw)
	}
}

// TestTheBannerShowsOnceAndIsGoneOnTheNextLoad pins the read-once rule. A
// banner that survived a reload would go on claiming a write just happened
// long after it did.
//
// It runs the write from the STATUS page, naming it as where the form came
// from, because that is what the quick overlay switches do: they post
// through the curated-setting handler and must land back on the fleet
// view, not on one camera's page.
func TestTheBannerShowsOnceAndIsGoneOnTheNextLoad(t *testing.T) {
	base, _, _ := osdWriteServer(t, osdBefore, osdAfter)
	c := flashBrowser(t)

	resp := postForm(t, c, base+"/cameras/cam1/settings", url.Values{
		"return": {"/"},
		"block":  {"osd get"},
		"xpath":  {"OsdChannelName/name"},
		"value":  {"lounge"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Fatalf("redirected to %q, want the status page the toggle came from", got)
	}

	first := getPage(t, c, base+"/")
	if !strings.Contains(first, `class="flash flash-confirmed"`) {
		t.Fatalf("the status page shows no confirmed banner:\n%s", first)
	}
	if !strings.Contains(first, "cam1: camera name saved") {
		t.Fatalf("the banner does not say what was saved:\n%s", first)
	}
	if !strings.Contains(first, ">undo<") {
		t.Fatalf("the banner offers no undo:\n%s", first)
	}

	second := getPage(t, c, base+"/")
	if strings.Contains(second, "flash-confirmed") || strings.Contains(second, "cam1: camera name saved") {
		t.Fatalf("the banner survived a reload:\n%s", second)
	}
}

// undoForm pulls the undo form's action and fields out of a rendered
// banner, so a test posts exactly what a browser would rather than a
// request it composed itself.
func undoForm(t *testing.T, page string) (action string, form url.Values) {
	t.Helper()
	block := regexp.MustCompile(`(?s)<form class="flash-undo" method="post" action="([^"]+)">(.*?)</form>`).FindStringSubmatch(page)
	if block == nil {
		t.Fatalf("no undo form on the page:\n%s", page)
	}
	action = html.UnescapeString(block[1])
	form = url.Values{}
	for _, m := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllStringSubmatch(block[2], -1) {
		form.Set(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return action, form
}

// TestUndoPostsThePreviousDocumentThroughTheVerifiedWritePath proves undo
// is the same restore result.html always offered, only presented smaller:
// the camera's own previous document, sent back through /write/{id} with
// verification on, not a value this page composed.
func TestUndoPostsThePreviousDocumentThroughTheVerifiedWritePath(t *testing.T) {
	pair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("osd get pair not found")
	}
	// Two writes happen in this test, each with its own dials: the curated
	// write (pre-read, write, verify) and then the undo through serveWrite
	// (write, verify). Five dials in all, in that order.
	writeCam, verifyCam := buildApplySettingFixtures(t, pair, osdBefore, osdAfter, 200)
	key := baichuan.AESKey(testProbeNonce, "")
	undoFixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	undoFixture = append(undoFixture, statusReply(t, key, pair.Get, 200, testXMLHeader+osdAfter)...)
	undoFixture = append(undoFixture, statusReply(t, key, pair.Set, 200, "")...)
	undoCam := fakecam.New(t, undoFixture)
	undoVerify := fakecam.New(t, append(loginHandshake(testProbeNonce, testProbeDeviceInfo),
		statusReply(t, key, pair.Get, 200, testXMLHeader+osdBefore)...))

	calls := 0
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		calls++
		addr := writeCam.Addr()
		switch calls {
		case 3:
			addr = verifyCam.Addr()
		case 4:
			addr = undoCam.Addr()
		case 5:
			addr = undoVerify.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := flashBrowser(t)

	postForm(t, c, ts.URL+"/cameras/cam1/settings", url.Values{
		"return": {"/"},
		"block":  {"osd get"},
		"xpath":  {"OsdChannelName/name"},
		"value":  {"lounge"},
	})
	action, form := undoForm(t, getPage(t, c, ts.URL+"/"))

	if want := "/cameras/cam1/write/" + strconv.FormatUint(uint64(pair.Set), 10); action != want {
		t.Fatalf("undo posts to %q, want the same verified write path %q", action, want)
	}
	if form.Get("verify") != "true" {
		t.Fatalf("undo does not ask for verification: %v", form)
	}
	if !strings.Contains(form.Get("body"), "<name>front</name>") {
		t.Fatalf("undo does not carry the camera's own previous document: %q", form.Get("body"))
	}

	resp := postForm(t, c, ts.URL+action, form)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("undo answered %d: %s", resp.StatusCode, raw)
	}
	// The proof is what reached the camera, not what the page claims: the
	// previous document, byte for byte, on a real SetConfig.
	sent := decodedSetConfigBody(t, undoCam.Received(), pair.Set, key)
	if !strings.Contains(sent, "<name>front</name>") {
		t.Fatalf("undo sent the camera something other than its previous document: %s", sent)
	}
}

// TestARefusedCuratedWriteReportsAsRefusedNotAsSuccess is the case the
// inferred image XPaths make realistic: a curated field whose path this
// camera's document does not carry. Condensing the report must not soften
// it -- a refusal is still a refusal, in the refused colour, carrying the
// reason.
func TestARefusedCuratedWriteReportsAsRefusedNotAsSuccess(t *testing.T) {
	pair, ok := pairForName("isp get")
	if !ok {
		t.Fatal("isp get pair not found")
	}
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
	c := flashBrowser(t)

	resp := postForm(t, c, ts.URL+"/cameras/cam1/settings", url.Values{
		"return": {"/"},
		"block":  {"isp get"},
		"xpath":  {"InputAdvanceCfg/DayNight/IrcutMode"},
		"value":  {"ir"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", resp.StatusCode)
	}

	page := getPage(t, c, ts.URL+"/")
	if !strings.Contains(page, `class="flash flash-refused"`) {
		t.Fatalf("a refused write does not report as refused:\n%s", page)
	}
	if strings.Contains(page, "flash-confirmed") || strings.Contains(page, "flash-accepted") {
		t.Fatalf("a refused write must never read as any kind of success:\n%s", page)
	}
	if !strings.Contains(page, "refused:") || !strings.Contains(page, "element not found in document") {
		t.Fatalf("a refusal must still carry its reason:\n%s", page)
	}
	// A refusal must not offer to undo a write that never happened.
	if strings.Contains(page, "flash-undo") {
		t.Fatalf("a refused write offered an undo:\n%s", page)
	}
}

// TestTheRawBlockEditorStillRendersTheFullBeforeAndAfterPage guards the
// one call site this change deliberately left alone. Seeing the exact
// before and after XML is the entire point of the Advanced page, and a
// one-line banner would destroy it.
func TestTheRawBlockEditorStillRendersTheFullBeforeAndAfterPage(t *testing.T) {
	pair := pickTestPair(t)
	wrote := "<body><field>new-value</field></body>"
	writeCam := fakecam.New(t, buildWriteFixture(t, pair, "<body><field>old-value</field></body>", 200))
	verifyCam := fakecam.New(t, buildReadBackFixture(t, pair, wrote))

	calls := 0
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		calls++
		addr := writeCam.Addr()
		if calls == 2 {
			addr = verifyCam.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := newTestHTTPServer(t, s)

	resp, err := http.PostForm(ts+"/cameras/cam1/write/"+strconv.FormatUint(uint64(pair.Set), 10),
		url.Values{"body": {wrote}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: the raw editor must still render its own page", resp.StatusCode)
	}
	for _, want := range []string{"<h2>Before</h2>", "<h2>After</h2>", "old-value", "new-value", "restore previous"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the raw block editor's result page no longer shows %q:\n%s", want, page)
		}
	}
}

// TestTheFlashStoreDoesNotGrowWithoutBound is the memory half of this. Each
// entry holds a camera's whole previous document, and an operator who
// submits a write and then closes the tab never comes back for theirs.
func TestTheFlashStoreDoesNotGrowWithoutBound(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1")})
	now := time.Now()
	s.flashNow = func() time.Time { return now }

	for i := range 500 {
		stash(s, fmt.Sprintf("abandoned-%d", i), Flash{Outcome: "confirmed", Message: "x"})
	}
	if got := s.flashLen(); got != 500 {
		t.Fatalf("store holds %d entries, want 500", got)
	}

	now = now.Add(flashTTL + time.Second)
	stash(s, "fresh", Flash{Outcome: "confirmed", Message: "x"})
	if got := s.flashLen(); got != 1 {
		t.Fatalf("store holds %d entries after every earlier one expired, want 1", got)
	}
}

// TestTheFlashStoreIsSafeUnderConcurrentUse: many requests touch this map
// at once on a fleet page, so the -race build has to be clean.
func TestTheFlashStoreIsSafeUnderConcurrentUse(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1")})
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		id := fmt.Sprintf("browser-%d", i)
		go func() {
			defer wg.Done()
			stash(s, id, Flash{Outcome: "confirmed", Message: id})
		}()
		go func() {
			defer wg.Done()
			s.takeFlash(requestWithFlashID(id))
		}()
	}
	wg.Wait()
}

// stash sets a flash as if it came from a browser already carrying the
// fallback cookie with this id, which is what an install running with no
// password looks like on its second request.
func stash(s *Server, id string, f Flash) {
	s.setFlash(httptest.NewRecorder(), requestWithFlashID(id), f)
}

func requestWithFlashID(id string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.AddCookie(&http.Cookie{Name: flashCookie, Value: id})
	return r
}

// TestReturnToAcceptsOnlyAPathOnThisPage. The status page's toggles name
// where they came from in a form field, and a form field is whatever the
// request says it is: an absolute URL there would turn a write handler
// into an open redirect.
func TestReturnToAcceptsOnlyAPathOnThisPage(t *testing.T) {
	const fallback = "/cameras/cam1"
	for _, tc := range []struct{ in, want string }{
		{"", fallback},
		{"/", "/"},
		{"/cameras/other", "/cameras/other"},
		{"https://elsewhere.example/", fallback},
		{"//elsewhere.example/", fallback},
		{"/\\elsewhere.example/", fallback},
		{"/ok\r\nX-Injected: 1", fallback},
		{"cameras/cam1", fallback},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{"return": {tc.in}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := returnTo(r, fallback); got != tc.want {
			t.Errorf("returnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
