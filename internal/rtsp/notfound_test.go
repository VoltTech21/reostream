package rtsp

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// pwnamte turned on only the sub stream of a D400 and pointed Frigate at the
// bare camera name. The 404 should say the camera is there under _sub.
func TestNotFoundNamesTheStreamThatIsServed(t *testing.T) {
	s := New("127.0.0.1:8554")
	sub := s.Add("regal_podstreha_sub")
	s.Add("gate") // registered, never ready: listed, marked so
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Close)
	feedUntilReady(t, sub, captureFrames(t))

	for path, want := range map[string]string{
		"/regal_podstreha":        "/regal_podstreha_sub",
		"/regal_podstreha_extern": "/regal_podstreha_sub",
		"/gate_sub":               "/gate (no video yet)",
		"/nothing":                "",
	} {
		res := s.notFound(path)
		if res.StatusCode != 404 {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
		if want == "" {
			if res.StatusMessage != "" {
				t.Errorf("%s: unexpected hint %q", path, res.StatusMessage)
			}
			continue
		}
		if !strings.Contains(res.StatusMessage, "serves "+want+";") {
			t.Errorf("%s: status line %q, want it to name %s", path, res.StatusMessage, want)
		}
	}
}

// A TrackMix with no zoom streams set up: the 404 and the log line say how
// to turn the missing one on, not only what is served (YseanY, 11-02).
func TestNotFoundSaysHowToTurnTheStreamOn(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := New("127.0.0.1:8554")
	main := s.Add("driveway")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Close)
	feedUntilReady(t, main, captureFrames(t))

	res := s.notFound("/driveway_zoom_sub")
	if !strings.Contains(res.StatusMessage, "turn zoom_sub on in reostream") {
		t.Errorf("status line %q does not say how to serve it", res.StatusMessage)
	}
	for _, want := range []string{"/driveway_zoom_sub is not served", "Live tab", `add "zoom_sub" to the camera's streams and rtsp lists`} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log line is missing %q:\n%s", want, logged.String())
		}
	}
	logged.Reset()
	s.notFound("/driveway_sub")
	if !strings.Contains(logged.String(), "tick sub for this camera on reostream's Cameras page") {
		t.Errorf("log line for a missing sub stream:\n%s", logged.String())
	}
}

// cam-19's main slots were held by another reostream, so /cam-19 was set up
// but never had a keyframe. The RTSP 404 left it out and the HTTP .ts 404
// listed it, and asking for /cam-19 itself said to turn main on, which it
// was (11-04). Both 404s now list what is set up, marked when it has no
// video yet, and the stream that is on says it is waiting for the camera.
func TestNotFoundListsAStreamWithNoVideoYet(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := New("127.0.0.1:8554")
	s.Add("cam-19")
	sub := s.Add("cam-19_sub")
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Close)
	feedUntilReady(t, sub, captureFrames(t))

	res := s.notFound("/cam-19_zoom")
	if want := "Not Found: this camera serves /cam-19 (no video yet), /cam-19_sub; turn zoom on in reostream to serve /cam-19_zoom"; res.StatusMessage != want {
		t.Errorf("status line\n got %q\nwant %q", res.StatusMessage, want)
	}
	res = s.notFound("/cam-19")
	if res.StatusCode != 404 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if want := "Not Found: /cam-19 is on in reostream, but the camera has sent no video for it yet; this camera serves /cam-19_sub"; res.StatusMessage != want {
		t.Errorf("status line for the stream that is on\n got %q\nwant %q", res.StatusMessage, want)
	}
	if strings.Contains(logged.String(), "turn main on") || strings.Contains(logged.String(), "tick main") {
		t.Errorf("told to turn on a stream that is on:\n%s", logged.String())
	}
}
