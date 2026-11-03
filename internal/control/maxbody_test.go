package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every control route reads its body through maxBody: a handler gets an
// error rather than an unbounded body.
func TestControlPageCapsRequestBodies(t *testing.T) {
	var readErr error
	var n int
	h := limitBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		n, readErr = len(b), err
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/config", strings.NewReader(strings.Repeat("a", maxBody+1))))
	if readErr == nil || n > maxBody {
		t.Fatalf("read %d bytes with error %v, want a cut off at %d", n, readErr, maxBody)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/config", strings.NewReader("small=1")))
	if readErr != nil {
		t.Fatalf("a small body failed: %v", readErr)
	}
}
