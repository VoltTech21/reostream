package control_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
)

func newTestServer(t *testing.T, opts control.Options) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(control.New(opts).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestDashboardRedirectsToLoginWhenNotAuthenticated(t *testing.T) {
	ts := newTestServer(t, control.Options{Password: "hunter2"})
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

func TestLoginWithTheRightPasswordSetsASession(t *testing.T) {
	ts := newTestServer(t, control.Options{Password: "hunter2"})
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

func TestLoginWithTheWrongPasswordDoesNot(t *testing.T) {
	ts := newTestServer(t, control.Options{Password: "hunter2"})
	resp, err := http.PostForm(ts.URL+"/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "reostream_control_session" && ck.Value != "" {
			t.Fatal("a session cookie was set for a failed login")
		}
	}
}

func TestAllowNoPasswordServesWithoutLogin(t *testing.T) {
	ts := newTestServer(t, control.Options{AllowNoPassword: true})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
}

func TestSessionTokensDifferBetweenLogins(t *testing.T) {
	ts := newTestServer(t, control.Options{Password: "hunter2"})
	// A plain http.Client follows the 303 redirect and loses the Set-Cookie
	// header from the intermediate response, so stop it from following.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	tok := func() string {
		resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"hunter2"}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		for _, ck := range resp.Cookies() {
			if ck.Name == "reostream_control_session" {
				return ck.Value
			}
		}
		t.Fatal("no session cookie")
		return ""
	}
	if a, b := tok(), tok(); a == b {
		t.Fatal("two logins produced the same session token")
	}
}
