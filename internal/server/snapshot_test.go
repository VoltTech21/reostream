package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VoltTech21/reostream/internal/hub"
)

func TestKeyframeUnknownStreamIs404(t *testing.T) {
	s := New(StaticHubs(map[string]*hub.Hub{}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/nope.keyframe", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestKeyframeBeforeAnyVideoIs503(t *testing.T) {
	// A real stream name with nothing recorded yet: the stream exists, it
	// just has no keyframe to show, which is a different condition from
	// the name being wrong.
	h := hub.New(4)
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cam.keyframe", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestKeyframeServesTheMostRecentOne(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("h265", []byte{0, 0, 0, 1, 0x26, 1, 2, 3})
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cam.keyframe", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/H265" {
		t.Errorf("Content-Type %q, want video/H265", ct)
	}
	if codec := rec.Header().Get("X-Reostream-Codec"); codec != "h265" {
		t.Errorf("X-Reostream-Codec %q, want h265", codec)
	}
	want := []byte{0, 0, 0, 1, 0x26, 1, 2, 3}
	if string(rec.Body.Bytes()) != string(want) {
		t.Errorf("body = %x, want %x", rec.Body.Bytes(), want)
	}
}

func TestKeyframeH264ContentType(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("h264", []byte{0, 0, 0, 1, 0x65})
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cam.keyframe", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "video/H264" {
		t.Errorf("Content-Type %q, want video/H264", ct)
	}
}

func TestKeyframeHeadReportsStatusWithoutABody(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("h265", []byte{1, 2, 3})
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("HEAD", "/cam.keyframe", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD wrote a body of %d bytes", rec.Body.Len())
	}
}

func TestKeyframeRejectsPOST(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("h265", []byte{1, 2, 3})
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/cam.keyframe", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestKeyframeRouteDoesNotShadowTheStreamRoute(t *testing.T) {
	// A regression guard for the manual suffix dispatch in
	// serveStreamOrKeyframe: a .ts request must still reach serveStream,
	// not fall through to the keyframe 404/503 path.
	h := hub.New(4)
	s := New(StaticHubs(map[string]*hub.Hub{"cam": h}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/cam.ts", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 from the stream route", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type %q, want video/mp2t", ct)
	}
}
