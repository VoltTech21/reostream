package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/hub"
)

func TestStatusReportsEveryStream(t *testing.T) {
	// Status must list a stream that is down, not omit it. A missing entry
	// reads as "no such camera" when the truth is "camera is broken".
	s := New(map[string]*hub.Hub{"a": hub.New(4), "b": hub.New(4)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Streams map[string]struct {
			Connected bool `json:"connected"`
			Clients   int  `json:"clients"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, ok := body.Streams[name]; !ok {
			t.Errorf("stream %q missing from status", name)
		}
	}
}

func TestDroppedAudioSurfacesInStatus(t *testing.T) {
	// An ADPCM camera silently dropping its audio is exactly the condition
	// that went unnoticed in production for days; the count must reach
	// status, not just live inside internal/ts.
	h := hub.New(4)
	h.AddDroppedAudio(7)
	s := New(map[string]*hub.Hub{"a": h})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var body struct {
		Streams map[string]struct {
			DroppedAudio int `json:"dropped_audio"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Streams["a"].DroppedAudio; got != 7 {
		t.Fatalf("dropped_audio = %d, want 7", got)
	}
}

func TestDroppedAudioSurfacesInMetrics(t *testing.T) {
	h := hub.New(4)
	h.AddDroppedAudio(7)
	s := New(map[string]*hub.Hub{"a": h})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `reostream_stream_dropped_audio_total{stream="a"} 7`) {
		t.Errorf("metrics missing the dropped audio count\n%s", body)
	}
}

func TestMetricsAreValidPrometheusText(t *testing.T) {
	s := New(map[string]*hub.Hub{"a": hub.New(4)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"# HELP", "# TYPE", `reostream_stream_clients{stream="a"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}
