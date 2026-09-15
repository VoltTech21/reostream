package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is the measurement the throttle's claims rest on. Two rounds of
// reasoning about this code were wrong in ways a twenty-line harness caught
// at once -- a per-request delay was reported as 2 checks a second and was
// really 8,399 once the attacker used eight sockets instead of four -- so
// the rate is measured here rather than argued.
//
// What is counted is credentials actually COMPARED, which is what a brute
// force gets paid in: every 401 is one comparison against the real
// password.

// loginRate drives one source with a wrong password at the given
// concurrency for d, and returns comparisons per second.
func loginRate(t *testing.T, workers int, d time.Duration) float64 {
	t.Helper()
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})

	var checks atomic.Int64
	// Cancelling the context is how the run ends: at the end of the window
	// most workers are still queued, and a queue deep enough to meter an
	// attacker is by definition too deep to wait out. A real attacker's
	// abandoned requests behave exactly this way, and their claims stay
	// spent (see TestAnAbandonedAttemptStillSpendsItsPlace), which is why
	// giving up does not buy the rate back.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	handler := s.Handler()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req := httptest.NewRequest("POST", "/login",
					strings.NewReader(url.Values{"password": {"wrong"}}.Encode())).WithContext(ctx)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				// One source, as it is behind docker-proxy: the attacker's
				// key and the operator's key are the same one.
				req.RemoteAddr = "172.17.0.1:5000"
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code == http.StatusUnauthorized {
					checks.Add(1)
				}
			}
		}()
	}
	start := time.Now()
	time.Sleep(d)
	elapsed := time.Since(start)
	cancel()
	wg.Wait()
	return float64(checks.Load()) / elapsed.Seconds()
}

// The property: one source's rate is flat across worker counts. A design
// that meters per request rather than per source looks fine at one worker
// and collapses at eight, so one worker count proves nothing.
func TestTheLoginRateIsFlatAcrossWorkerCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("measures a rate over several seconds")
	}
	// Two seconds keeps the suite quick, and a short window measures the
	// ramp -- the first, unpenalised attempt and the doubling below the cap
	// -- more than the steady state. The steady state is the number the
	// password minimum is sized against, so it is worth being able to
	// measure it directly:
	//
	//	REOSTREAM_RATE_WINDOW=30s go test ./internal/control/ -run RateIsFlat -v
	//
	// The property under test is the same either way: flat across worker
	// counts.
	window := 2 * time.Second
	if v := os.Getenv("REOSTREAM_RATE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("REOSTREAM_RATE_WINDOW=%q: %v", v, err)
		}
		window = d
	}

	var rates []float64
	for _, workers := range []int{1, 4, 8, 64, 512} {
		r := loginRate(t, workers, window)
		rates = append(rates, r)
		t.Logf("workers=%-4d %8.2f checks/sec", workers, r)
	}

	// The ceiling: even the first, unpenalised attempt plus the ramp up to
	// throttleMaxDelay cannot exceed a handful of checks in the window, and
	// a design that skips the wait under concurrency lands in the
	// thousands. Generous, because this is a timing test on a shared
	// machine; it is the ORDER that is being asserted, and the order is
	// what every previous failure got wrong by three or four of them.
	const ceiling = 20.0
	for i, workers := range []int{1, 4, 8, 64, 512} {
		if rates[i] > ceiling {
			t.Errorf("workers=%d measured %.2f checks/sec, want at most %.0f: concurrency is buying guesses", workers, rates[i], ceiling)
		}
	}
	// And flat: the many-worker rates must not be a multiple of the single
	// worker's. Compared against a floor as well as the measurement, so a
	// slow machine that manages only a couple of checks either way does not
	// fail on a ratio between two small integers.
	base := rates[0]
	if base < 1 {
		base = 1
	}
	for i, workers := range []int{1, 4, 8, 64, 512} {
		if rates[i] > 4*base {
			t.Errorf("workers=%d measured %.2f checks/sec against %.2f at one worker: the rate is not flat", workers, rates[i], rates[0])
		}
	}
}

// The queue is what makes the rate flat, so assert its arithmetic directly
// rather than only through a timing measurement: N attempts arriving
// together take N different deadlines, spaced by the penalty.
func TestConcurrentAttemptsTakeDifferentPlacesInTheQueue(t *testing.T) {
	th, clock := newFakeThrottle()
	th.base = 100 * time.Millisecond
	th.fail("172.17.0.1")

	start := clock.now()
	var last time.Time
	for i := 0; i < 8; i++ {
		got, queued := th.claim("172.17.0.1")
		if !queued {
			t.Fatalf("attempt %d was not queued", i)
		}
		want := start.Add(time.Duration(i) * th.base)
		if !got.Equal(want) {
			t.Fatalf("attempt %d got deadline %v, want %v", i, got.Sub(start), want.Sub(start))
		}
		if i > 0 && !got.After(last) {
			t.Fatalf("attempt %d got a deadline that is not after attempt %d's", i, i-1)
		}
		last = got
	}

	// Claimed concurrently, the same must hold: eight goroutines, eight
	// distinct deadlines.
	th2, _ := newFakeThrottle()
	th2.base = 100 * time.Millisecond
	th2.fail("10.0.0.1")
	seen := make(chan time.Time, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, ok := th2.claim("10.0.0.1"); ok {
				seen <- d
			}
		}()
	}
	wg.Wait()
	close(seen)
	uniq := map[time.Time]bool{}
	n := 0
	for d := range seen {
		if uniq[d] {
			t.Fatalf("two attempts claimed the same deadline %v", d)
		}
		uniq[d] = true
		n++
	}
	if n != 32 {
		t.Fatalf("%d of 32 attempts were queued", n)
	}
}

// A claim is spent even if its request goes away. Otherwise opening
// connections and dropping them would buy the rate back.
func TestAnAbandonedAttemptStillSpendsItsPlace(t *testing.T) {
	th, clock := newFakeThrottle()
	th.base = time.Second
	th.fail("10.0.0.1")

	first, _ := th.claim("10.0.0.1")
	// That request gives up. Nothing hands the slot back.
	second, ok := th.claim("10.0.0.1")
	if !ok {
		t.Fatal("the next attempt was not queued")
	}
	if got, want := second.Sub(first), th.base; got != want {
		t.Fatalf("the attempt after an abandoned one waits %v behind it, want %v", got, want)
	}
	_ = clock
}

// A queue must not be a life sentence. A source that stops failing is
// forgotten after the window, penalty and queue position together.
func TestAQueueDecaysWithItsWindow(t *testing.T) {
	th, clock := newFakeThrottle()
	th.base = time.Second
	th.fail("10.0.0.1")
	for i := 0; i < 20; i++ {
		th.claim("10.0.0.1") // a deep queue, deadlines far ahead
	}
	if d, ok := th.claim("10.0.0.1"); !ok || d.Sub(clock.now()) < 10*time.Second {
		t.Fatalf("the queue did not build: next deadline is %v away", d.Sub(clock.now()))
	}

	clock.advance(throttleWindow + time.Second)
	if _, ok := th.claim("10.0.0.1"); ok {
		t.Fatal("a source that stopped failing is still queued after the window")
	}
	if th.len() != 0 {
		t.Fatalf("%d entries survived the window", th.len())
	}
}

// Whatever the queue, a success clears it: the operator who finally types
// the right password does not carry the attacker's queue into their next
// sign-in.
func TestASuccessClearsTheQueue(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	s.throttle.base = time.Millisecond
	key := throttleKey("172.17.0.1:5000")
	for i := 0; i < 4; i++ {
		s.throttle.fail(key)
		s.throttle.claim(key)
	}
	if rec := postLogin(t, s, "172.17.0.1:5000", "hunter2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d, want 303", rec.Code)
	}
	if s.throttle.len() != 0 {
		t.Fatalf("a successful login left %d entries behind", s.throttle.len())
	}
	if _, ok := s.throttle.claim(key); ok {
		t.Fatal("the queue survived a success")
	}
}

// What the escape hatch costs to reach, stated as a test rather than as a
// claim in a comment: every one of the 4096 slots is held by a request that
// is waiting, so an attacker needs that many in flight at once before a
// single attempt skips the queue.
func TestTheSleeperGuardOnlyOpensWhenEverySlotIsHeld(t *testing.T) {
	th, _ := newFakeThrottle()
	for i := 0; i < throttleMaxSleepers; i++ {
		if !th.beginSleep() {
			t.Fatalf("the guard refused slot %d of %d", i+1, throttleMaxSleepers)
		}
	}
	if th.beginSleep() {
		t.Fatal("the guard handed out a slot past its cap")
	}
	th.endSleep()
	if !th.beginSleep() {
		t.Fatal("releasing a slot did not free one")
	}
	// An unpaired release must not drive the counter negative and quietly
	// raise the cap.
	for i := 0; i < throttleMaxSleepers*2; i++ {
		th.endSleep()
	}
	th.mu.Lock()
	n := th.sleepers
	th.mu.Unlock()
	if n != 0 {
		t.Fatalf("the sleeper count is %d after over-releasing, want 0", n)
	}
}
