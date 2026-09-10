package control_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/control"
)

func TestCloseReleasesALiveLogStream(t *testing.T) {
	// http.Server.Shutdown blocks on active connections and does not cancel
	// their request contexts, so a subscriber to /logs/stream must be
	// released by Close, not by the request context, or a live tab would
	// hold shutdown open for its full timeout.
	srv := control.New(control.Options{AllowNoPassword: true, Logs: control.NewLogBuffer(10)})
	req := httptest.NewRequest(http.MethodGet, "/logs/stream", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	// Give serveLogStream time to reach its subscribe loop before Close.
	time.Sleep(50 * time.Millisecond)
	srv.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveLogStream did not return after Close")
	}
}
