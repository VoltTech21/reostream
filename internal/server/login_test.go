package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func get(t *testing.T, h http.Handler, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" || pass != "" {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// No login configured is the streaming port as it always was: open.
func TestNoLoginLeavesTheStreamsOpen(t *testing.T) {
	h := RequireLogin(okHandler(), "", "")
	if w := get(t, h, "/cam.ts", "", ""); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with no login configured", w.Code)
	}
}

// With a login, a stream needs it: none or the wrong one is a 401 with the
// challenge a player needs to ask for it, and the right one plays.
func TestALoginGuardsTheStreams(t *testing.T) {
	h := RequireLogin(okHandler(), "frigate", "s3cret")
	for _, path := range []string{"/cam.ts", "/cam_sub.ts", "/cam.keyframe", "/cam_sub.still.ts", "/cam.frames"} {
		w := get(t, h, path, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with no login: status %d, want 401", path, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: a 401 with no challenge, so a player never asks for the login", path)
		}
		if w := get(t, h, path, "frigate", "wrong"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s with the wrong password: status %d, want 401", path, w.Code)
		}
		if w := get(t, h, path, "someone", "s3cret"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s with the wrong username: status %d, want 401", path, w.Code)
		}
		if w := get(t, h, path, "frigate", "s3cret"); w.Code != http.StatusOK {
			t.Errorf("%s with the right login: status %d, want 200", path, w.Code)
		}
	}
}

// The health check and the metrics carry no picture and stay open, so a
// container's health check does not need the recorder's password.
func TestStatusAndMetricsStayOpenBehindALogin(t *testing.T) {
	h := RequireLogin(okHandler(), "frigate", "s3cret")
	for _, path := range []string{"/api/status", "/metrics"} {
		if w := get(t, h, path, "", ""); w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 without a login", path, w.Code)
		}
	}
}
