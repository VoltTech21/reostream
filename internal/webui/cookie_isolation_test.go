package webui_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/VoltTech21/reostream/internal/camctl"
	"github.com/VoltTech21/reostream/internal/control"
)

// TestCamctlAndControlUseDifferentSessionCookies is the regression for one
// cookie name shared by two processes on one host: reocam's camera control
// page and the streaming daemon's own operator page each run their own
// SessionStore, so a shared cookie name means logging into one silently
// logs the other surface out. Each must set a distinctly named cookie; the
// login route on neither surface touches the camera fleet config, so
// neither server here needs a real one.
func TestCamctlAndControlUseDifferentSessionCookies(t *testing.T) {
	camctlSrv, err := camctl.New(camctl.Options{Password: "hunter2", ConfigPath: "/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	camctlTS := httptest.NewServer(camctlSrv.Handler())
	t.Cleanup(camctlTS.Close)

	controlTS := httptest.NewServer(control.New(control.Options{Password: "hunter2"}).Handler())
	t.Cleanup(controlTS.Close)

	camctlName := loginCookieName(t, camctlTS.URL, "hunter2")
	controlName := loginCookieName(t, controlTS.URL, "hunter2")

	if camctlName == "" || controlName == "" {
		t.Fatalf("did not observe a session cookie from both surfaces: camctl=%q control=%q", camctlName, controlName)
	}
	if camctlName == controlName {
		t.Fatalf("camctl and control both set a cookie named %q: logging into one silently logs the other out", camctlName)
	}
}

func loginCookieName(t *testing.T, base, password string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		return ""
	}
	return cookies[0].Name
}
