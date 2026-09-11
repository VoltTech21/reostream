package camctl

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

// newTestServer builds a camctl server for a test, failing the test rather
// than returning an error, so callers read as tests and not as plumbing.
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeTestConfig writes a reostream config naming the given cameras and
// returns its path. Addresses are from the documentation range, never a real
// one, so a test that accidentally dials cannot reach anything.
func writeTestConfig(t *testing.T, names ...string) string {
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
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t)})
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
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t)})
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
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t)})
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
		if ck.Name == "reostream_session" && ck.Value != "" {
			t.Fatal("a session cookie was set for a failed login")
		}
	}
}

func TestAllowNoPasswordServesWithoutLogin(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t)})
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
