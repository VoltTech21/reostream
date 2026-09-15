package control

import (
	"context"
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

// fakeClock lets a test step over the failure window without sleeping
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

// newFakeThrottle is a throttle with a clock a test drives and a base delay
// short enough that a test which really waits one out does not cost a
// second.
func newFakeThrottle() (*throttle, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	th := newThrottle()
	th.now = c.now
	th.base = time.Millisecond
	return th, c
}

// The delay doubles per consecutive failure and stops at the cap. It is a
// delay and not a lockout on purpose: behind docker-proxy every request
// shares one key, so a refusal would let any passer-by lock the operator
// out of their own page.
func TestTheDelayGrowsWithFailuresAndCaps(t *testing.T) {
	th, clock := newFakeThrottle()
	th.base = throttleBaseDelay

	if d := th.delay("10.0.0.1"); d != 0 {
		t.Fatalf("a source that has never failed waits %v, want 0", d)
	}

	want := throttleBaseDelay
	for i := 0; i < 20; i++ {
		th.fail("10.0.0.1")
		got := th.delay("10.0.0.1")
		if want > throttleMaxDelay {
			want = throttleMaxDelay
		}
		if got != want {
			t.Fatalf("after %d failures the delay is %v, want %v", i+1, got, want)
		}
		want *= 2
	}
	if d := th.delay("10.0.0.1"); d != throttleMaxDelay {
		t.Fatalf("the delay settled at %v, want the cap %v", d, throttleMaxDelay)
	}
	// Another source is unaffected: the delay is per source, not global.
	if d := th.delay("10.0.0.2"); d != 0 {
		t.Fatalf("an unrelated source waits %v, want 0", d)
	}
	// And failures expire.
	clock.advance(throttleWindow + time.Second)
	if d := th.delay("10.0.0.1"); d != 0 {
		t.Fatalf("after the window the delay is %v, want 0", d)
	}
	if th.len() != 0 {
		t.Fatalf("an expired entry was kept: %d entries", th.len())
	}
}

func TestASuccessClearsTheDelay(t *testing.T) {
	th, _ := newFakeThrottle()
	for i := 0; i < 4; i++ {
		th.fail("10.0.0.1")
	}
	th.clear("10.0.0.1")
	if th.len() != 0 {
		t.Fatal("a success left the failures behind")
	}
	th.fail("10.0.0.1")
	if got := th.delay("10.0.0.1"); got != th.base {
		t.Fatalf("after a success the next failure costs %v, want the first-failure delay %v", got, th.base)
	}
}

// One attacker holding a /64 -- which is what an ISP hands a single
// customer -- must not get 2^64 independent delay budgets, nor 2^64 keys to
// push everyone else's entry out of the table with.
func TestOneIPv6PrefixIsOneSource(t *testing.T) {
	a := throttleKey("[2001:db8:1:2::1]:5000")
	b := throttleKey("[2001:db8:1:2:ffff:ffff:ffff:ffff]:5000")
	if a != b {
		t.Fatalf("two addresses in one /64 keyed as %q and %q, want one key", a, b)
	}
	// A different /64 is a different source.
	if c := throttleKey("[2001:db8:1:3::1]:5000"); c == a {
		t.Fatalf("a different /64 shares the key %q", c)
	}
	// IPv4 is charged per address, and the ephemeral port never counts.
	if x, y := throttleKey("192.168.1.10:5000"), throttleKey("192.168.1.10:41234"); x != y {
		t.Fatalf("two connections from one IPv4 address keyed as %q and %q", x, y)
	}
	if x, y := throttleKey("192.168.1.10:5000"), throttleKey("192.168.1.11:5000"); x == y {
		t.Fatalf("two IPv4 addresses share the key %q", x)
	}
	// An IPv4-mapped IPv6 peer is the IPv4 address, not a /64 of them.
	if got, want := throttleKey("[::ffff:192.168.1.10]:5000"), "192.168.1.10"; got != want {
		t.Fatalf("an IPv4-mapped peer keyed as %q, want %q", got, want)
	}
	// Unparseable sources are charged as themselves rather than skipped.
	if got := throttleKey("not-an-address"); got != "not-an-address" {
		t.Fatalf("an unparseable source keyed as %q", got)
	}
}

// The map is keyed by an attacker-controlled value, so it has to be
// bounded: a flood of distinct sources would otherwise grow it forever, and
// a brute-force fix would have bought a memory-exhaustion hole.
func TestTheThrottleMapStaysBoundedUnderAFloodOfSources(t *testing.T) {
	th, _ := newFakeThrottle()
	for i := 0; i < throttleMax*3; i++ {
		th.fail(fmt.Sprintf("2001:db8:%x::/64", i))
		if n := th.len(); n > throttleMax {
			t.Fatalf("after %d distinct sources the table holds %d entries, cap is %d", i+1, n, throttleMax)
		}
	}
}

// The per-key sleeping limit is what actually meters a brute force. A
// delay alone does not: nothing serialises attempts, so a hundred
// concurrent guesses would all wait the same two seconds and all be
// judged. Only four attempts from one source may be sleeping at once --
// and the ones past that are CHECKED WITHOUT THE DELAY, never refused,
// because behind a shared hop the attacker's key is the operator's.
func TestOneSourceCanOnlyHoldItsShareOfSleepers(t *testing.T) {
	th, _ := newFakeThrottle()
	th.base = time.Hour // long enough that nothing finishes on its own
	th.fail("10.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < throttleMaxSleepersPerKey; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			th.wait(ctx, "10.0.0.1")
		}()
	}
	waitForSleepers(t, th, throttleMaxSleepersPerKey)

	// The next attempt from that same source returns at once, having
	// skipped the delay rather than joined the queue or been refused.
	done := make(chan error, 1)
	go func() { done <- th.wait(ctx, "10.0.0.1") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the attempt past the per-key limit returned %v, want nil: it must be checked, not refused", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the attempt past the per-key limit queued instead of skipping the delay")
	}
	th.mu.Lock()
	n, held := th.sleepers, th.perKey["10.0.0.1"]
	th.mu.Unlock()
	if n != throttleMaxSleepersPerKey || held != throttleMaxSleepersPerKey {
		t.Fatalf("one source holds %d sleepers (%d globally), want %d", held, n, throttleMaxSleepersPerKey)
	}

	// A different source is unaffected: one key cannot monopolise the pool.
	th.fail("10.0.0.2")
	wg.Add(1)
	go func() {
		defer wg.Done()
		th.wait(ctx, "10.0.0.2")
	}()
	waitForSleepers(t, th, throttleMaxSleepersPerKey+1)

	// Cancelled clients release their slots rather than pinning goroutines,
	// and the per-key table keeps nothing behind.
	cancel()
	wg.Wait()
	th.mu.Lock()
	n, keys := th.sleepers, len(th.perKey)
	th.mu.Unlock()
	if n != 0 || keys != 0 {
		t.Fatalf("%d sleepers and %d per-key entries survived their cancelled requests", n, keys)
	}
}

// waitForSleepers blocks until the throttle reports exactly n sleepers.
func waitForSleepers(t *testing.T, th *throttle, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		th.mu.Lock()
		got := th.sleepers
		th.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sleepers, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// The global cap is a goroutine guard and nothing more, so reaching it must
// not cost anybody their sign-in: an attempt that cannot get a slot skips
// the delay and is checked.
func TestTheGlobalSleeperCapNeverRefuses(t *testing.T) {
	th, _ := newFakeThrottle()
	th.base = time.Hour
	// Fill every slot, spread across enough sources to respect the per-key
	// limit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < throttleMaxSleepers/throttleMaxSleepersPerKey; i++ {
		key := fmt.Sprintf("10.1.%d.%d", i/256, i%256)
		th.fail(key)
		for j := 0; j < throttleMaxSleepersPerKey; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				th.wait(ctx, key)
			}()
		}
	}
	waitForSleepers(t, th, throttleMaxSleepers)

	// One more source, with failures of its own: no slot left, so it is
	// checked immediately rather than turned away.
	th.fail("10.2.0.1")
	done := make(chan error, 1)
	go func() { done <- th.wait(ctx, "10.2.0.1") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an attempt at the global cap returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an attempt at the global cap queued instead of skipping the delay")
	}
	cancel()
	wg.Wait()
}

// A cancelled request stops waiting immediately: a client that gives up
// must not leave a goroutine asleep on its behalf.
func TestACancelledRequestStopsWaiting(t *testing.T) {
	th, _ := newFakeThrottle()
	th.base = time.Hour
	th.fail("10.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- th.wait(ctx, "10.0.0.1") }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled wait reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled wait kept sleeping")
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
				th.delay(key)
				th.wait(context.Background(), key)
				if j%5 == 0 {
					th.clear(key)
				}
			}
		}(i)
	}
	wg.Wait()
}

// postLogin submits a password from a chosen source.
func postLogin(t *testing.T, s *Server, remoteAddr, pw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/login",
		strings.NewReader(url.Values{"password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The property the whole redesign turns on: however many times a source has
// failed, the RIGHT password still works. Behind docker-proxy that source
// is everybody, so a refusal here would be a denial of service any
// passer-by could impose on the operator.
func TestTheRightPasswordStillWorksAfterManyFailures(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	s.throttle.base = time.Millisecond

	for i := 0; i < 4; i++ {
		if rec := postLogin(t, s, "172.17.0.1:5000", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d, want 401", i+1, rec.Code)
		}
	}
	// Straight to the cap from here, rather than spending the real seconds
	// it would take to walk up to it through the handler.
	key := throttleKey("172.17.0.1:5000")
	for s.throttle.delay(key) < throttleMaxDelay {
		s.throttle.fail(key)
	}
	// The operator, arriving through the same shared hop, still gets in.
	if rec := postLogin(t, s, "172.17.0.1:5000", "hunter2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d, want 303", rec.Code)
	}
	if s.throttle.len() != 0 {
		t.Fatal("a successful login left failures behind")
	}
}

// A failed login costs the next attempt from that source a delay, and it is
// charged before the password is looked at, so a wrong guess cannot be
// compared and retried at full speed.
func TestAFailedLoginChargesTheNextAttempt(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	s.throttle.base = time.Millisecond

	key := throttleKey("203.0.113.7:5000")
	if d := s.throttle.delay(key); d != 0 {
		t.Fatalf("a first attempt is charged %v, want 0", d)
	}
	postLogin(t, s, "203.0.113.7:5000", "wrong")
	if d := s.throttle.delay(key); d != s.throttle.base {
		t.Fatalf("after one failure the next attempt is charged %v, want %v", d, s.throttle.base)
	}
	postLogin(t, s, "203.0.113.7:5000", "wrong")
	if d := s.throttle.delay(key); d != 2*s.throttle.base {
		t.Fatalf("after two failures the next attempt is charged %v, want %v", d, 2*s.throttle.base)
	}
}

// The claim token is 79.3 bits, so throttling it buys nothing and would
// hand a passer-by a way to slow the operator's own claim. Wrong tokens
// must therefore cost nothing at all.
func TestAWrongTokenCostsNoDelay(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      filepath.Join(t.TempDir(), "config.toml"),
	})
	for i := 0; i < 10; i++ {
		if rec := postClaimFrom(t, s, "172.17.0.1:5000", "WRNG-TKEN-WRNG-TKEN", "pw"); rec.Code != http.StatusForbidden {
			t.Fatalf("wrong token %d answered %d, want 403", i+1, rec.Code)
		}
	}
	if s.throttle.len() != 0 {
		t.Fatalf("the claim route recorded %d throttle entries; it must record none", s.throttle.len())
	}
	// And the real token still claims, immediately, from that same source.
	if rec := postClaimFrom(t, s, "172.17.0.1:5000", s.claimToken(), "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right token answered %d after ten wrong ones, want 303", rec.Code)
	}
}

// The whole point, at the level an operator experiences it: a correct
// password works while the page is saturated with attempts. Under the old
// design this was a 503, which behind a shared hop meant an attacker
// flooding the login locked the operator out of it -- and the flood that
// did it was also their optimal guessing strategy.
func TestTheRightPasswordWorksWhileTheSleeperPoolIsSaturated(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	// Long enough that nothing in the pool finishes during the test.
	s.throttle.base = time.Hour
	s.throttle.fail(throttleKey("172.17.0.1:5000"))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < throttleMaxSleepersPerKey; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/login",
				strings.NewReader(url.Values{"password": {"wrong"}}.Encode())).WithContext(ctx)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = "172.17.0.1:5000"
			s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	waitForSleepers(t, s.throttle, throttleMaxSleepersPerKey)

	// The operator, arriving through the very same shared hop, is let in
	// without waiting on anybody.
	rec := postLogin(t, s, "172.17.0.1:5000", "hunter2")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d while the pool was saturated, want 303", rec.Code)
	}

	cancel()
	wg.Wait()
}
