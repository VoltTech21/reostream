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

// TestGetHonorsContextCancellation proves a caller's own deadline actually
// aborts a call in flight, not just the gap between calls: against a
// handler that never answers, Get must return once ctx expires rather than
// waiting out the Client's own 20 second http.Client timeout. This is the
// test a fix that threads ctx into do() only cosmetically (never handing it
// to the *http.Request) would fail: such code would still block until the
// handler answers or the http.Client timeout fires, long past ctx's own
// deadline.
func TestGetHonorsContextCancellation(t *testing.T) {
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
		t.Fatalf("Get took %v to return after a 50ms context deadline; the request was not actually canceled", elapsed)
	}
}

// A camera allows only a handful of concurrent CGI logins and holds an
// abandoned one for minutes, so a session that is finished with has to be
// handed back. The symptom when it is not: "max session (rspCode -5)" on
// every read, from the Reolink app and the camera's own web UI as much as
// from here, after a few minutes of ordinary browsing.
func TestCloseHandsTheSessionBack(t *testing.T) {
	var logouts int32
	var sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok1")))
		case "Logout":
			atomic.AddInt32(&logouts, 1)
			sawToken = r.URL.Query().Get("token")
			w.Write([]byte(`[{"cmd":"Logout","code":0,"value":{"rspCode":200}}]`))
		default:
			t.Errorf("unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	defer srv.Close()

	c, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if logouts != 1 {
		t.Errorf("logouts = %d, want 1", logouts)
	}
	if sawToken != "tok1" {
		t.Errorf("Logout carried token %q, want the session's own tok1", sawToken)
	}

	// Closing twice must not log out twice: Close sits under an
	// unconditional defer, and a second one would be a command against a
	// session the camera has already dropped.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if logouts != 1 {
		t.Errorf("after a second Close, logouts = %d, want 1", logouts)
	}

	// And a nil client, so a defer placed before the error check is not a
	// panic waiting to happen.
	var nilClient *Client
	if err := nilClient.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

// A camera with HTTP turned off still answers over HTTPS, with a
// self-signed certificate.
func TestDialFallsBackToHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(loginReply("tls")))
	}))
	defer srv.Close()

	c, err := Dial(strings.TrimPrefix(srv.URL, "https://"), "admin", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if !strings.HasPrefix(c.base, "https://") {
		t.Errorf("base = %q, want https", c.base)
	}
}

// A camera that answers HTTP and refuses the login is not retried over
// HTTPS: the refusal is the answer.
func TestDialDoesNotFallBackOnRefusal(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(`[{"cmd":"Login","code":1,"error":{"detail":"invalid user","rspCode":-27}}]`))
	}))
	defer srv.Close()

	_, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "wrong")
	if err == nil || !strings.Contains(err.Error(), "invalid user") {
		t.Fatalf("err = %v, want the camera's refusal", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// Neither scheme reachable says how to fix it.
func TestDialNamesPortSettingsWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	_, err := Dial(host, "admin", "")
	if err == nil || !strings.Contains(err.Error(), "Port Settings") {
		t.Fatalf("err = %v, want a hint about Port Settings", err)
	}
	// What lets the floodlight and the siren go over Baichuan instead.
	if !Unreachable(err) {
		t.Fatalf("err = %v is not reported as no web API", err)
	}
}

// A camera that answers and refuses is not one with no web API: going
// round it over Baichuan would hide a wrong password.
func TestARefusedLoginIsNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"cmd":"Login","code":1,"error":{"detail":"invalid user","rspCode":-27}}]`))
	}))
	defer srv.Close()
	_, err := Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "wrong")
	if err == nil || Unreachable(err) {
		t.Fatalf("err = %v, want a refusal that is not unreachable", err)
	}
}
