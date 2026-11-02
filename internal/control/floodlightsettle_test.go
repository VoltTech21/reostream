package control

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// The light changes at once, but GetWhiteLed can report the old state for
// most of a second after the write. That was reported as "reads back as
// motion" while the light was on (fake hardware, 10-30, bug 4); the
// read-back now waits for it, each time on a fresh session.
func TestFloodlightReadBackWaitsForTheCameraToCatchUp(t *testing.T) {
	old := floodlightSettle
	floodlightSettle = 600 * time.Millisecond
	t.Cleanup(func() { floodlightSettle = old })

	var mu sync.Mutex
	state, stale, logins := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch cmd := r.URL.Query().Get("cmd"); cmd {
		case "Login":
			logins++
			w.Write([]byte(loginReply("tok")))
		case "Logout":
			w.Write([]byte(`[{"cmd":"Logout","code":0,"value":{"rspCode":200}}]`))
		case "GetWhiteLed":
			s := state
			if stale > 0 {
				stale--
				s = 0
			}
			w.Write([]byte(`[{"cmd":"GetWhiteLed","code":0,"value":{"WhiteLed":{"mode":1,"state":` + strconv.Itoa(s) + `}}}]`))
		case "SetWhiteLed":
			state, stale = 1, 2
			w.Write([]byte(`[{"cmd":"SetWhiteLed","code":0}]`))
		default:
			t.Errorf("unexpected cmd %q", cmd)
		}
	}))
	t.Cleanup(srv.Close)
	cgiDial := func(Camera) (*cgi.Client, error) {
		return cgi.Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp := postForm(t, flashBrowser(t), ts.URL+"/cameras/cam1/floodlight", url.Values{"option": {"on"}})
	banner := takenFlash(t, s, resp)
	if banner.Outcome != "confirmed" || strings.Contains(banner.Message, "reads back as") {
		t.Fatalf("banner %+v, want confirmed once the camera reports on", banner)
	}
	mu.Lock()
	defer mu.Unlock()
	if logins < 3 {
		t.Errorf("%d logins: the read-back did not take fresh sessions", logins)
	}
}
