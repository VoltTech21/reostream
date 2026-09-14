package control_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/control"
)

// writeTestConfig writes a reostream config naming the given cameras and
// returns its path. Addresses are from the documentation range, never a
// real one, so a test that accidentally dials cannot reach anything. This
// mirrors routes_test.go's own writeTestConfig: that one lives in package
// control, unreachable from here.
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

// noDial refuses immediately rather than dialling anything, the same
// discipline cgistub_test.go's noCGIDial follows in package control: this
// test's camera has a documentation-range address, but serveCamera would
// still attempt a real dial against it without a double.
func noDial(context.Context, control.Camera) (*baichuan.Conn, error) {
	return nil, fmt.Errorf("baichuan not available in this test")
}

// noCGIDial is package control_test's own copy of cgistub_test.go's
// stub, needed for the same reason: serveCamera reads the floodlight over
// CGI on every visit, so any test hitting that page needs a double or it
// tries a real network dial against the documentation-range address.
func noCGIDial(control.Camera) (*cgi.Client, error) {
	return nil, fmt.Errorf("cgi not available in this test")
}

// getBody performs the request through the server and returns the body as
// a string, failing the test on a non-200.
func getBody(t *testing.T, s *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200: %s", path, resp.StatusCode, body)
	}
	return string(body)
}

func TestCameraPageShowsStreamStateAndSettingsTogether(t *testing.T) {
	// A person has one camera in their head, not a config entry and a
	// device. The page must carry both aspects.
	s := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Dial:            noDial,
		CGIDial:         noCGIDial,
		Status: fixedStatus{
			"one/main": {Connected: true, Streaming: true, FPS: 24.9, BitrateBps: 6.2e6},
		},
	})
	body := getBody(t, s, "/cameras/one")

	for _, want := range []string{"streaming", "24.9", "6.2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("camera page is missing its stream state: no %q", want)
		}
	}
	if !strings.Contains(body, "Settings") {
		t.Fatal("camera page does not carry its settings")
	}
}
