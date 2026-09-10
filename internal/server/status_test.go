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
	s := New(StaticHubs(map[string]*hub.Hub{"a": hub.New(4), "b": hub.New(4)}))
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
	s := New(StaticHubs(map[string]*hub.Hub{"a": h}))
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
	s := New(StaticHubs(map[string]*hub.Hub{"a": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `reostream_stream_dropped_audio_total{stream="a"} 7`) {
		t.Errorf("metrics missing the dropped audio count\n%s", body)
	}
}

func TestMetricsAreValidPrometheusText(t *testing.T) {
	s := New(StaticHubs(map[string]*hub.Hub{"a": hub.New(4)}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"# HELP", "# TYPE", `reostream_stream_clients{stream="a"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestThreeAudioStatesReportDistinctlyInStatus(t *testing.T) {
	// Every stream now declares an audio track whether or not its camera
	// sends one, so ffprobe and dropped_audio alone cannot tell a silent
	// camera from a healthy one. audio_frames is what closes that gap:
	// this asserts /api/status actually reports the three states as three
	// different combinations, not just that the field exists.
	silent := hub.New(4)
	dropping := hub.New(4)
	dropping.AddDroppedAudio(9)
	healthy := hub.New(4)
	healthy.AddAudioFrames(9)

	s := New(StaticHubs(map[string]*hub.Hub{"silent": silent, "dropping": dropping, "healthy": healthy}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var body struct {
		Streams map[string]struct {
			AudioFrames  int `json:"audio_frames"`
			DroppedAudio int `json:"dropped_audio"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if st := body.Streams["silent"]; st.AudioFrames != 0 || st.DroppedAudio != 0 {
		t.Errorf("silent: audio_frames=%d dropped_audio=%d, want both 0", st.AudioFrames, st.DroppedAudio)
	}
	if st := body.Streams["dropping"]; st.AudioFrames != 0 || st.DroppedAudio != 9 {
		t.Errorf("dropping: audio_frames=%d dropped_audio=%d, want 0 and 9", st.AudioFrames, st.DroppedAudio)
	}
	if st := body.Streams["healthy"]; st.AudioFrames != 9 || st.DroppedAudio != 0 {
		t.Errorf("healthy: audio_frames=%d dropped_audio=%d, want 9 and 0", st.AudioFrames, st.DroppedAudio)
	}
}

func TestAudioFramesSurfacesInMetrics(t *testing.T) {
	h := hub.New(4)
	h.AddAudioFrames(9)
	s := New(StaticHubs(map[string]*hub.Hub{"a": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `reostream_stream_audio_frames_total{stream="a"} 9`) {
		t.Errorf("metrics missing the audio frame count\n%s", body)
	}
}
