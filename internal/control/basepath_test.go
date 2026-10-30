package control

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/config"
)

// noCamera stands in for every camera dial in these tests, so a page that
// would reach for a camera renders its "could not reach" state instead of
// waiting on a timeout.
func noCamera(o Options) Options {
	o.Dial = func(context.Context, Camera) (*baichuan.Conn, error) {
		return nil, errors.New("no camera in this test")
	}
	o.CGIDial = func(Camera) (*cgi.Client, error) {
		return nil, errors.New("no camera in this test")
	}
	return o
}

func TestCleanBase(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"/":                            "",
		"/reostream":                   "/reostream",
		"/reostream/":                  "/reostream",
		" /a/b/ ":                      "/a/b",
		"/api/hassio_ingress/Ab-9_x.~": "/api/hassio_ingress/Ab-9_x.~",
		"reostream":                    "",
		"//evil.example":               "",
		"/a/../b":                      "",
		"/a/..":                        "",
		`/a"onload=x`:                  "",
		"/a b":                         "",
		"/a\r\nSet-Cookie: x":          "",
		"/a?b":                         "",
	} {
		if got := cleanBase(in); got != want {
			t.Errorf("cleanBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAtPrefixesOnlyRootPaths(t *testing.T) {
	for _, c := range []struct{ base, in, want string }{
		{"", "/cameras/one", "/cameras/one"},
		{"/p", "/cameras/one", "/p/cameras/one"},
		{"/p", "/p/cameras/one", "/p/cameras/one"},
		{"/p", "/p", "/p"},
		{"/p", "/pp", "/p/pp"},
		{"/p", "http://192.0.2.1:8560/one_sub.ts", "http://192.0.2.1:8560/one_sub.ts"},
		{"/p", "//elsewhere/x", "//elsewhere/x"},
		{"/p", "relative", "relative"},
		{"/p", "", ""},
	} {
		if got := at(c.base, c.in); got != c.want {
			t.Errorf("at(%q, %q) = %q, want %q", c.base, c.in, got, c.want)
		}
	}
}

// rootURL finds a URL in a rendered page that starts at "/" without
// starting at the prefix: in an attribute, or as a string literal a script
// starts a URL with. Either would send a browser behind a proxy out of the
// page, to whatever the proxy serves at the root.
var (
	rootAttrRE = regexp.MustCompile(`\s([a-zA-Z][\w-]*)="(/[^"]*)"`)
	rootJSRE   = regexp.MustCompile("(?:[(=:]\\s*)['`](/[a-zA-Z][^'`]*)")
	// A "return" field is a root path on purpose: it is read by this
	// server, which prefixes the redirect it becomes. See baseWriter.
	returnFieldRE = regexp.MustCompile(`name="return" value="/[^"]*"`)
)

func escapesBase(page, base string) []string {
	page = returnFieldRE.ReplaceAllString(page, "")
	var bad []string
	for _, m := range rootAttrRE.FindAllStringSubmatch(page, -1) {
		attr, v := m[1], m[2]
		// data-also holds path patterns the rail compares against a path
		// it has already taken the prefix off; see layout.html.
		if attr == "data-also" || attr == "placeholder" {
			continue
		}
		if !strings.HasPrefix(v, base+"/") && v != base {
			bad = append(bad, attr+`="`+v+`"`)
		}
	}
	for _, m := range rootJSRE.FindAllStringSubmatch(page, -1) {
		if !strings.HasPrefix(m[1], base+"/") {
			bad = append(bad, m[0])
		}
	}
	return bad
}

// TestPagesUnderAPrefixStayUnderIt renders every page an operator can open
// under a prefix and checks none of them links, posts, loads or fetches
// outside it. It goes through the router, so a page added later is
// covered as soon as it is in routePatterns.
func TestPagesUnderAPrefixStayUnderIt(t *testing.T) {
	const base = "/proxy/reo"
	cfg := writeTestConfig(t, "one", "two")
	s := newTestServer(t, noCamera(Options{ConfigPath: cfg, AllowNoPassword: true, BasePath: base}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	rendered := 0
	for _, pattern := range routePatterns {
		method, path, _ := strings.Cut(pattern, " ")
		if method != "GET" || strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/stream/") ||
			strings.HasPrefix(path, "/logs/") || strings.HasSuffix(path, "/talk") ||
			strings.Contains(path, "/recordings/") || path == "/settings/events/recent" ||
			strings.HasSuffix(path, "/osd") {
			continue // not pages: data, media, a stream or a socket
		}
		path = strings.NewReplacer("{name}", "one", "{$}", "", "{cat}", "image").Replace(path)
		t.Run(pattern, func(t *testing.T) {
			resp, err := c.Get(ts.URL + base + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if loc := resp.Header.Get("Location"); loc != "" && !strings.HasPrefix(loc, base+"/") {
				t.Fatalf("redirected to %q, outside %s", loc, base)
			}
			// A redirect's own body is http.Redirect's one-line link, which
			// no browser shows; the Location above is what it follows.
			if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				return
			}
			rendered++
			for _, b := range escapesBase(string(body), base) {
				t.Errorf("page links outside %s: %s", base, b)
			}
		})
	}
	// A walk that rendered nothing would pass without having looked.
	if rendered < 15 {
		t.Fatalf("only %d pages rendered under the prefix; the walk is not reaching them", rendered)
	}
}

// The other categories of a camera's settings render different sections,
// each with its own forms; the route walk above only visits one.
func TestEveryCameraSettingsCategoryStaysUnderThePrefix(t *testing.T) {
	const base = "/p"
	s := newTestServer(t, noCamera(Options{ConfigPath: writeTestConfig(t, "one"), AllowNoPassword: true, BasePath: base}))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for _, cat := range []string{"image", "streams", "audio", "lights", "detection", "ptz", "network", "clips", "view", "stitch", "system"} {
		resp, err := http.Get(ts.URL + base + "/cameras/one/settings/" + cat)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		for _, b := range escapesBase(string(body), base) {
			t.Errorf("%s: page links outside %s: %s", cat, base, b)
		}
	}
}

// TestTemplatesAndScriptsCarryThePrefix reads the sources rather than
// rendered pages, so a URL behind an {{if}} no test data reaches is
// checked too. A template writes {{base}} or {{at ...}} in front of a
// root path; a script under assets/ builds on pageBase.
func TestTemplatesAndScriptsCarryThePrefix(t *testing.T) {
	attr := regexp.MustCompile(`\s(href|src|action|formaction|data-[\w-]+)="/`)
	js := regexp.MustCompile("(?:[(=:]\\s*)['`]/[a-zA-Z]")
	fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(templateFS, p)
		for i, line := range strings.Split(string(b), "\n") {
			line = regexp.MustCompile(`data-also="[^"]*"`).ReplaceAllString(line, "")
			line = returnFieldRE.ReplaceAllString(line, "")
			if attr.MatchString(line) || js.MatchString(line) {
				t.Errorf("%s:%d writes a root URL without {{base}}: %s", p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	fs.WalkDir(assetFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, "mpegts.js") {
			return err
		}
		b, _ := fs.ReadFile(assetFS, p)
		for i, line := range strings.Split(string(b), "\n") {
			if js.MatchString(line) {
				t.Errorf("%s:%d starts a root URL without pageBase: %s", p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
}

func TestRedirectsAndCookiesKeepThePrefix(t *testing.T) {
	const base = "/p"
	s := newTestServer(t, Options{Password: "correct horse battery", ConfigPath: writeClaimedConfig(t, "correct horse battery"), BasePath: base})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := c.Get(ts.URL + base + "/cameras")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != base+"/login" {
		t.Fatalf("signed-out redirect went to %q, want %s/login", got, base)
	}

	resp, err = c.PostForm(ts.URL+base+"/login", url.Values{"password": {"correct horse battery"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != base+"/" {
		t.Fatalf("sign-in redirect went to %q, want %s/", got, base)
	}
	cks := resp.Cookies()
	if len(cks) == 0 || cks[0].Path != base+"/" {
		t.Fatalf("session cookie %v, want Path=%s/", cks, base)
	}

	// A proxy that strips the prefix before forwarding reaches the same
	// routes, and the redirects still carry it for the browser.
	resp, err = c.Get(ts.URL + "/cameras")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != base+"/login" {
		t.Fatalf("stripped-prefix redirect went to %q, want %s/login", got, base)
	}
}

func TestNoPrefixLeavesThePageAtTheRoot(t *testing.T) {
	s := newTestServer(t, Options{ConfigPath: writeTestConfig(t, "one"), AllowNoPassword: true})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/cameras")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`href="/wall"`, `href="/assets/style.css"`, `<meta name="reostream-base" content="">`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page at the root lacks %s", want)
		}
	}
}

// writeClaimedConfig is a config whose [control] section holds a password,
// for a test that needs the sign-in flow rather than allow_no_password.
func writeClaimedConfig(t *testing.T, password string) string {
	t.Helper()
	p := writeTestConfig(t)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(b), "allow_no_password = true", "password = \""+password+"\"", 1)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ingressRequest is a request as Home Assistant's ingress proxy sends it.
func ingressRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = ingressProxyAddr + ":40000"
	r.Header.Set("X-Ingress-Path", "/api/hassio_ingress/tok-123")
	r.Header.Set("X-Forwarded-Host", "homeassistant.local:8123")
	return r
}

func TestIngressIsTrustedOnlyFromTheSupervisor(t *testing.T) {
	const pw = "correct horse battery"
	cfg := writeClaimedConfig(t, pw)

	// From the Supervisor, on an install that trusts it: no sign-in, and
	// the page links under the ingress prefix.
	s := newTestServer(t, Options{Password: pw, ConfigPath: cfg, HAIngress: true})
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, ingressRequest("GET", "/cameras"))
	if w.Code != http.StatusOK {
		t.Fatalf("ingress request answered %d (Location %q), want 200", w.Code, w.Header().Get("Location"))
	}
	if bad := escapesBase(w.Body.String(), "/api/hassio_ingress/tok-123"); len(bad) > 0 {
		t.Fatalf("ingress page links outside its prefix: %v", bad)
	}

	// The same headers from anywhere else are just headers.
	r := ingressRequest("GET", "/cameras")
	r.RemoteAddr = "192.0.2.50:40000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("spoofed ingress answered %d (Location %q), want 303 /login", w.Code, w.Header().Get("Location"))
	}

	// And an install that was not told to trust ingress trusts nobody.
	s = newTestServer(t, Options{Password: pw, ConfigPath: cfg})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, ingressRequest("GET", "/cameras"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("ingress without -ha-ingress answered %d (Location %q), want 303 /login", w.Code, w.Header().Get("Location"))
	}
}

func TestIngressRedirectsKeepTheIngressPrefix(t *testing.T) {
	s := newTestServer(t, Options{ConfigPath: writeTestConfig(t), AllowNoPassword: true, HAIngress: true})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, ingressRequest("GET", "/"))
	if got := w.Header().Get("Location"); got != "/api/hassio_ingress/tok-123/setup" {
		t.Fatalf("no-cameras redirect went to %q, want the ingress prefix's /setup", got)
	}
}

func TestIngressRefusesACrossSitePost(t *testing.T) {
	s := newTestServer(t, Options{ConfigPath: writeTestConfig(t, "one"), AllowNoPassword: true, HAIngress: true})
	h := s.Handler()

	r := ingressRequest("POST", "/fleet/order")
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST through ingress answered %d, want 403", w.Code)
	}

	r = ingressRequest("POST", "/fleet/order")
	r.Header.Set("Origin", "http://homeassistant.local:8123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatalf("same-site POST through ingress was refused")
	}
}

// An unclaimed install opened through Home Assistant is claimed by that
// first request, with a password nobody was shown, so the page on its own
// port stops answering without one.
func TestIngressClaimsAnUnclaimedInstall(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.toml"
	s := newTestServer(t, Options{ConfigPath: cfg, AllowNoPassword: true, HAIngress: true, ControlListen: "0.0.0.0:8562"})
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if got := w.Header().Get("Location"); got != "/claim" {
		t.Fatalf("direct request to an unclaimed install went to %q, want /claim", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, ingressRequest("GET", "/"))
	if got := w.Header().Get("Location"); got != "/api/hassio_ingress/tok-123/setup" {
		t.Fatalf("first ingress request went to %q, want the setup page", got)
	}
	loaded, err := config.LoadRaw(cfg)
	if err != nil {
		t.Fatalf("ingress did not write a config: %v", err)
	}
	if loaded.Control == nil || len(loaded.Control.Password) < claimPasswordMinLength {
		t.Fatalf("ingress claim wrote no usable password: %+v", loaded.Control)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cameras", nil))
	if got := w.Header().Get("Location"); got != "/login" {
		t.Fatalf("direct request after the ingress claim went to %q, want /login", got)
	}

	// Through ingress the password page sets a password without the old
	// one, which nobody knows.
	r := ingressRequest("POST", "/password")
	r.Body = io.NopCloser(strings.NewReader(url.Values{"new": {"a brand new passphrase"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Location"); got != "/api/hassio_ingress/tok-123/" {
		t.Fatalf("password change through ingress answered %d (Location %q): %s", w.Code, got, w.Body.String())
	}
	if !s.authNow().Check("a brand new passphrase") {
		t.Fatal("the password set through ingress is not the one the page now asks for")
	}
}
