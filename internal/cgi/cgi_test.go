package cgi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// loginReply is what a camera sends back for a successful Login.
func loginReply(token string) string {
	return `[{"cmd":"Login","code":0,"value":{"Token":{"name":"` + token + `","leaseTime":3600}}}]`
}

// notLoggedIn is the error a camera returns when it has forgotten the
// session, which it does intermittently on a connection that just logged in.
const notLoggedIn = `[{"cmd":"GetWhiteLed","code":1,"error":{"detail":"please login first","rspCode":-6}}]`

func TestGetRetriesAfterSessionExpiry(t *testing.T) {
	var logins, gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			n := atomic.AddInt32(&logins, 1)
			w.Write([]byte(loginReply("tok" + string(rune('0'+n)))))
		case "GetWhiteLed":
			// Fail the first read the way a camera does, then answer.
			if atomic.AddInt32(&gets, 1) == 1 {
				w.Write([]byte(notLoggedIn))
				return
			}
			w.Write([]byte(`[{"cmd":"GetWhiteLed","code":0,"value":{"WhiteLed":{"mode":1,"state":0}}}]`))
		default:
			t.Errorf("unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	defer srv.Close()

	c, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	value, _, _, err := c.Get(context.Background(), "GetWhiteLed", 0)
	if err != nil {
		t.Fatalf("Get after session expiry: %v", err)
	}
	var got struct {
		WhiteLed struct{ Mode int } `json:"WhiteLed"`
	}
	if err := json.Unmarshal(value, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.WhiteLed.Mode != 1 {
		t.Errorf("mode = %d, want 1", got.WhiteLed.Mode)
	}
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (the initial one and the retry)", logins)
	}
}

// A -6 that keeps coming back must not retry forever.
func TestGetGivesUpOnRepeatedSessionExpiry(t *testing.T) {
	var logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmd") == "Login" {
			atomic.AddInt32(&logins, 1)
			w.Write([]byte(loginReply("tok")))
			return
		}
		w.Write([]byte(notLoggedIn))
	}))
	defer srv.Close()

	c, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, _, err := c.Get(context.Background(), "GetWhiteLed", 0); err == nil {
		t.Fatal("expected an error when the camera never accepts the session")
	}
	if logins > 2 {
		t.Errorf("logins = %d, want at most 2: the retry must not loop", logins)
	}
}

// An error that is not a session problem must surface as itself.
func TestNonSessionErrorIsNotRetried(t *testing.T) {
	var logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmd") == "Login" {
			atomic.AddInt32(&logins, 1)
			w.Write([]byte(loginReply("tok")))
			return
		}
		w.Write([]byte(`[{"cmd":"GetWhiteLed","code":1,"error":{"detail":"not support","rspCode":-9}}]`))
	}))
	defer srv.Close()

	c, _ := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	_, _, _, err := c.Get(context.Background(), "GetWhiteLed", 0)
	if err == nil || !strings.Contains(err.Error(), "not support") {
		t.Fatalf("err = %v, want the camera's own message", err)
	}
	if logins != 1 {
		t.Errorf("logins = %d, want 1: an unsupported command is not a session problem", logins)
	}
}

// TestGetHonoursContextCancellation proves a caller's own deadline actually
// aborts a call in flight, not just the gap between calls: against a
// handler that never answers, Get must return once ctx expires rather than
// waiting out the Client's own 20 second http.Client timeout. This is the
// test a fix that threads ctx into do() only cosmetically (never handing it
// to the *http.Request) would fail: such code would still block until the
// handler answers or the http.Client timeout fires, long past ctx's own
// deadline.
func TestGetHonoursContextCancellation(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmd") == "Login" {
			w.Write([]byte(loginReply("tok")))
			return
		}
		<-unblock
		w.Write([]byte(`[{"cmd":"GetWhiteLed","code":0,"value":{"WhiteLed":{"mode":0,"state":0}}}]`))
	}))
	t.Cleanup(func() {
		close(unblock)
		srv.Close()
	})

	c, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, _, err = c.Get(ctx, "GetWhiteLed", 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Get against a handler that never answers returned no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Get took %v to return after a 50ms context deadline; the request was not actually cancelled", elapsed)
	}
}
