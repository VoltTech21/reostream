package control

import (
	"fmt"
	"io"
	"io/fs"
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
//
// The config carries a [control] section saying allow_no_password, because
// that is what a CLAIMED install looks like, and a test server exists to
// exercise the pages behind the claim gate. A config with no [control]
// section at all is a different state -- it answers neither question
// claimed() asks, so it leaves the install unclaimed and every route
// redirecting to the claim screen. See claim.go, and
// TestAConfigWithNoControlSectionLeavesTheInstallUnclaimed, which is the
// test that owns that state.
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

// Every route except the login pair and the static assets must be behind
// the auth wrapper. An unauthenticated route on this page reaches cameras.
func TestEveryRouteExceptLoginIsAuthenticated(t *testing.T) {
	protected := []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/cameras"},
		{"GET", "/cameras/one"},
		{"POST", "/cameras/one/settings"},
		{"GET", "/cameras/one/osd"},
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

// The stylesheet must be served to a request with no session, or the login
// page -- the one screen an operator cannot get past without it -- renders
// unstyled. This is a deliberate exception and it is only defensible while
// nothing under assets/ is a secret, so the test asserts that too: the
// whole directory is walked, and every file must be readable without a
// session and must not carry anything drawn from this install.
func TestAssetsAreServedWithoutASession(t *testing.T) {
	// A distinctive name, not "one": the check below is a substring
	// search across files that include vendored JavaScript and a licence,
	// and a common word would match by coincidence rather than by leak.
	const cameraName = "qv7-marker-cam"
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, cameraName)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	names, err := fs.Glob(assetSub, "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no assets are embedded at all")
	}
	for _, name := range names {
		resp, err := c.Get(ts.URL + "/assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/assets/%s answered %d to a request with no session, want 200", name, resp.StatusCode)
			continue
		}
		// Nothing under assets/ may be derived from this install: they
		// are the same bytes in every copy of the image.
		for _, secret := range []string{"hunter2", cameraName, s.opts.ConfigPath, s.claimToken()} {
			if secret != "" && strings.Contains(string(body), secret) {
				t.Errorf("/assets/%s carries %q, which is specific to this install", name, secret)
			}
		}
	}

	// And the stylesheet in particular, by the URL the layout links.
	resp, err := c.Get(ts.URL + "/assets/style.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stylesheet the login page links answered %d, want 200", resp.StatusCode)
	}
}
