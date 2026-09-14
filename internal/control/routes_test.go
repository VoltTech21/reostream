package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer builds a control server for a test, failing the test
// rather than returning an error, so callers read as tests and not as
// plumbing.
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeTestConfig writes a reostream config naming the given cameras and
// returns its path. Addresses are from the documentation range, never a
// real one, so a test that accidentally dials cannot reach anything.
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

// Every route except the login pair must be behind the auth wrapper. An
// unauthenticated route on this page reaches cameras.
func TestEveryRouteExceptLoginIsAuthenticated(t *testing.T) {
	protected := []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/cameras"},
		{"GET", "/cameras/one"},
		{"POST", "/cameras/one/settings"},
		{"POST", "/cameras/one/floodlight"},
		{"GET", "/cameras/one/time"},
		{"POST", "/cameras/one/time"},
		{"GET", "/cameras/one/accounts"},
		{"GET", "/cameras/one/advanced"},
		{"POST", "/cameras/one/write/45"},
		{"POST", "/cameras/add"},
		{"GET", "/fleet/apply"},
		{"POST", "/fleet/apply/ntp"},
		{"POST", "/fleet/apply/timezone"},
		{"GET", "/logs"},
		{"GET", "/logs/history"},
		{"GET", "/logs/stream"},
		{"GET", "/config"},
		{"POST", "/config"},
		{"GET", "/setup"},
		{"GET", "/setup/urls"},
		{"POST", "/setup/probe"},
		{"GET", "/assets/style.css"},
	}

	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, r := range protected {
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
				t.Errorf("%s %s returned %d, want 303 to the login page", r.method, r.path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
				t.Errorf("%s %s redirected to %q, want /login", r.method, r.path, loc)
			}
		})
	}
}
