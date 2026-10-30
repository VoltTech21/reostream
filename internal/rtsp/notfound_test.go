package rtsp

import (
	"strings"
	"testing"
)

// pwnamte turned on only the sub stream of a D400 and pointed Frigate at the
// bare camera name. The 404 should say the camera is there under _sub.
func TestNotFoundNamesTheStreamThatIsServed(t *testing.T) {
	s := New("127.0.0.1:8554")
	sub := s.Add("regal_podstreha_sub")
	s.Add("gate") // registered, never ready: not offered
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Close)
	feedUntilReady(t, sub, captureFrames(t))

	for path, want := range map[string]string{
		"/regal_podstreha":        "/regal_podstreha_sub",
		"/regal_podstreha_extern": "/regal_podstreha_sub",
		"/gate_sub":               "",
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
		if !strings.Contains(res.StatusMessage, "serves "+want+")") {
			t.Errorf("%s: status line %q, want it to name %s", path, res.StatusMessage, want)
		}
	}
}
