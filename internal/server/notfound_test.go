package server

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/hub"
)

// The HTTP .ts 404 for a stream the camera does not have turned on gives
// the same "how to turn it on" hint the RTSP 404 does (11-02): a plain 404
// for /cam-19_zoom_sub.ts left the reader nothing to go on. The log line
// is written once per path, and a name that is no camera stays a plain 404.
// A stream that is on but has sent nothing yet (cam-19 main, its slots
// held elsewhere) is listed and marked, in the same words the RTSP 404
// uses (11-04).
func TestHTTPNotFoundSaysHowToTurnTheStreamOn(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	sub := hub.New(4)
	sub.RecordFrame(1000)
	s := New(StaticHubs(map[string]*hub.Hub{"cam-19/main": hub.New(4), "cam-19/sub": sub}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := get("/cam-19_zoom_sub.ts")
	if code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", code)
	}
	for _, want := range []string{"this camera serves /cam-19.ts (no video yet), /cam-19_sub.ts.", "Live tab", `add "zoom_sub" to the camera's streams and rtsp lists`} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body is missing %q:\n%s", want, body)
		}
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log line is missing %q:\n%s", want, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "/cam-19_zoom_sub.ts is not served") {
		t.Errorf("log line does not name the path:\n%s", logged.String())
	}
	logged.Reset()
	get("/cam-19_zoom_sub.ts")
	if logged.Len() != 0 {
		t.Errorf("the hint was logged again for the same path:\n%s", logged.String())
	}
	if _, body := get("/cam-19_extern.ts"); !strings.Contains(body, "tick extern for this camera on reostream's Cameras page") {
		t.Errorf("404 for a missing extern:\n%s", body)
	}
	logged.Reset()
	if code, body := get("/nosuch_sub.ts"); code != http.StatusNotFound || strings.Contains(body, "serves") || logged.Len() != 0 {
		t.Errorf("a name that is no camera: %d %q, logged %q", code, body, logged.String())
	}
}
