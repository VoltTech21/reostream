package control

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCameraTestServer builds a camera control server for a test, failing
// the test rather than returning an error, so callers read as tests and not
// as plumbing.
func newCameraTestServer(t *testing.T, opts CameraOptions) *CameraServer {
	t.Helper()
	s, err := NewCameraServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeCameraTestConfig writes a reostream config naming the given cameras and
// returns its path. Addresses are from the documentation range, never a real
// one, so a test that accidentally dials cannot reach anything.
func writeCameraTestConfig(t *testing.T, names ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("listen = \"0.0.0.0:8560\"\n")
	for i, name := range names {
		fmt.Fprintf(&b, "\n[[camera]]\nname = %q\naddress = \"192.0.2.%d\"\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n", name, i+10)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnauthenticatedRequestRedirectsToLogin(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{Password: "hunter2", ConfigPath: writeCameraTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("redirected to %q, want /login", got)
	}
}

func TestCorrectPasswordSetsASessionCookie(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{Password: "hunter2", ConfigPath: writeCameraTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", resp.StatusCode)
	}

	u, _ := url.Parse(ts.URL)
	if len(jar.Cookies(u)) == 0 {
		t.Fatal("no session cookie was set")
	}
}

func TestWrongPasswordReturns401AndSetsNoUsableCookie(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{Password: "hunter2", ConfigPath: writeCameraTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.PostForm(ts.URL+"/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "reostream_camctl_session" && ck.Value != "" {
			t.Fatal("a session cookie was set for a failed login")
		}
	}
}

// protectedRoutes is every route Handler registers except the two login
// routes, kept next to Handler on purpose: TestEveryNonLoginRouteRequiresAuth
// below walks this list rather than introspecting the mux (Go's ServeMux
// exposes no way to enumerate its own patterns), so a route added to
// Handler without a matching line here is a gap this test cannot see.
// Handler's own comment says everything but login runs behind auth.Wrap;
// this is what actually checks that claim, route by route, rather than
// trusting it.
var protectedRoutes = []struct{ method, path string }{
	{"GET", "/"},
	{"GET", "/fleet/apply"},
	{"POST", "/fleet/apply/ntp"},
	{"POST", "/fleet/apply/timezone"},
	{"GET", "/camera/cam1"},
	{"GET", "/camera/cam1/blocks"},
	{"GET", "/camera/cam1/settings"},
	{"POST", "/camera/cam1/settings"},
	{"POST", "/camera/cam1/floodlight"},
	{"GET", "/camera/cam1/time"},
	{"POST", "/camera/cam1/time"},
	{"GET", "/camera/cam1/accounts"},
	{"POST", "/camera/cam1/write/45"},
}

// TestEveryNonLoginRouteRequiresAuth is the regression for a route added to
// Handler without wrapping it in s.auth.Wrap: TestUnauthenticatedRequestRedirectsToLogin
// only ever checked GET /, so a future route registered with plain
// mux.HandleFunc instead of mux.Handle(pattern, s.auth.Wrap(...)) would have
// shipped unnoticed. Every route in protectedRoutes must redirect an
// unauthenticated request to /login exactly as GET / already does.
func TestEveryNonLoginRouteRequiresAuth(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{Password: "hunter2", ConfigPath: writeCameraTestConfig(t, "cam1")})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, r := range protectedRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req, err := http.NewRequest(r.method, ts.URL+r.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("got %d, want 303 (redirect to /login): this route is reachable with no session", resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/login" {
				t.Fatalf("redirected to %q, want /login", got)
			}
		})
	}
}

func TestCameraAllowNoPasswordServesWithoutLogin(t *testing.T) {
	s := newCameraTestServer(t, CameraOptions{AllowNoPassword: true, ConfigPath: writeCameraTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
}
