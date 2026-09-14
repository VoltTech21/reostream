package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock lets a test step over the lockout window without sleeping
// through five real minutes.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFakeThrottle() (*throttle, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	th := newThrottle()
	th.now = c.now
	return th, c
}

func TestTheThrottleLocksOutAfterRepeatedFailures(t *testing.T) {
	th, clock := newFakeThrottle()
	for i := 0; i < throttleFailures-1; i++ {
		th.fail("10.0.0.1")
		if _, blocked := th.blocked("10.0.0.1"); blocked {
			t.Fatalf("locked out after %d failures, want %d", i+1, throttleFailures)
		}
	}
	th.fail("10.0.0.1")
	left, blocked := th.blocked("10.0.0.1")
	if !blocked {
		t.Fatalf("not locked out after %d failures", throttleFailures)
	}
	if left <= 0 || left > throttleWindow {
		t.Fatalf("lockout has %v left, want between 0 and %v", left, throttleWindow)
	}
	// Somebody else is unaffected: the lockout is per source, not global,
	// or one attacker would lock the operator out of their own page.
	if _, blocked := th.blocked("10.0.0.2"); blocked {
		t.Fatal("one source's failures locked out another")
	}
	// And it ends.
	clock.advance(throttleWindow + time.Second)
	if _, blocked := th.blocked("10.0.0.1"); blocked {
		t.Fatal("the lockout outlived its window")
	}
	if th.len() != 0 {
		t.Fatalf("an expired entry was kept: %d entries", th.len())
	}
}

func TestASuccessClearsTheThrottle(t *testing.T) {
	th, _ := newFakeThrottle()
	for i := 0; i < throttleFailures-1; i++ {
		th.fail("10.0.0.1")
	}
	th.clear("10.0.0.1")
	if th.len() != 0 {
		t.Fatal("a success left the failures behind")
	}
	// The count really is back to zero, not merely hidden: another full run
	// of failures short of the limit still does not lock out.
	for i := 0; i < throttleFailures-1; i++ {
		th.fail("10.0.0.1")
	}
	if _, blocked := th.blocked("10.0.0.1"); blocked {
		t.Fatal("the cleared failures were still being counted")
	}
}

// The map is keyed by an attacker-controlled value, so it is a
// memory-growth vector unless it is bounded: a flood of distinct sources
// (trivial from one IPv6 /64) would otherwise grow it forever, and a
// brute-force fix would have bought a memory-exhaustion hole.
func TestTheThrottleMapStaysBoundedUnderAFloodOfSources(t *testing.T) {
	th, _ := newFakeThrottle()
	for i := 0; i < throttleMax*3; i++ {
		th.fail(fmt.Sprintf("2001:db8::%x", i))
		if n := th.len(); n > throttleMax {
			t.Fatalf("after %d distinct sources the table holds %d entries, cap is %d", i+1, n, throttleMax)
		}
	}
	if th.len() > throttleMax {
		t.Fatalf("table holds %d entries, cap is %d", th.len(), throttleMax)
	}
}

// Requests arrive concurrently, so the table has to be safe under -race.
func TestTheThrottleIsRaceFree(t *testing.T) {
	th, _ := newFakeThrottle()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				key := fmt.Sprintf("10.0.%d.%d", i, j%7)
				th.fail(key)
				th.blocked(key)
				if j%5 == 0 {
					th.clear(key)
				}
			}
		}(i)
	}
	wg.Wait()
}

// End to end on the route that matters: the token is the only thing between
// a stranger who can reach the port and an install that can write to
// cameras, so guessing it has to stop being free.
func TestRepeatedWrongTokensLockOutTheClaimRoute(t *testing.T) {
	s, tok := unclaimedServer(t)
	for i := 0; i < throttleFailures; i++ {
		if rec := postClaimFrom(t, s, "203.0.113.5:5000", "WRNG-TKEN-WRNG-TKEN", "pw"); rec.Code != http.StatusForbidden {
			t.Fatalf("wrong token %d answered %d, want 403", i+1, rec.Code)
		}
	}
	rec := postClaimFrom(t, s, "203.0.113.5:5000", "WRNG-TKEN-WRNG-TKEN", "pw")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the attempt past the limit answered %d, want 429", rec.Code)
	}
	// Even the RIGHT token is refused while the lockout stands: a guesser
	// who stumbles onto it on attempt six must not be let in, and the check
	// runs before the compare so there is nothing to time either.
	if rec := postClaimFrom(t, s, "203.0.113.5:5000", tok, "pw"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a locked-out source with the right token answered %d, want 429", rec.Code)
	}
	// Another source is unaffected, and claims.
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("an unrelated source answered %d, want 303", rec.Code)
	}
}

// One mistyped password must not cost an operator their next four.
func TestTheLoginThrottleLocksOutAndASuccessClearsIt(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})

	login := func(pw string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/login",
			strings.NewReader(url.Values{"password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "203.0.113.7:5000"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Short of the limit, then right: the failures are forgotten.
	for i := 0; i < throttleFailures-1; i++ {
		if rec := login("wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d, want 401", i+1, rec.Code)
		}
	}
	if rec := login("hunter2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d, want 303", rec.Code)
	}
	if s.throttle.len() != 0 {
		t.Fatal("a successful login left failures behind")
	}

	// Past the limit, locked out -- and the right password does not get in
	// while it stands.
	for i := 0; i < throttleFailures; i++ {
		if rec := login("wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d, want 401", i+1, rec.Code)
		}
	}
	if rec := login("wrong"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the attempt past the limit answered %d, want 429", rec.Code)
	}
	if rec := login("hunter2"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a locked-out source with the right password answered %d, want 429", rec.Code)
	}
}

// One throttle, shared, so a guesser cannot collect a fresh allowance per
// route. The two routes cannot both be reached in one state of the install
// -- an unclaimed one redirects /login to the claim screen, a claimed one
// 404s /claim -- so what is asserted here is that the claim route's
// failures land in the same table serveLogin consults.
func TestTheClaimAndTheLoginShareOneThrottle(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      filepath.Join(t.TempDir(), "config.toml"),
	})
	for i := 0; i < throttleFailures; i++ {
		postClaimFrom(t, s, "203.0.113.9:5000", "WRNG-TKEN-WRNG-TKEN", "pw")
	}
	if _, blocked := s.throttle.blocked("203.0.113.9"); !blocked {
		t.Fatal("wrong tokens did not lock the source out of the shared throttle")
	}
}
