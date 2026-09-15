package control

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// throttle meters how often one source may have a login password CHECKED,
// without ever refusing a correct one.
//
// It is a per-key leaky bucket, and the shape matters more than any of the
// numbers. Each arriving attempt claims the next slot in its source's
// queue: under one lock it takes deadline = max(now, next[key]) and moves
// next[key] to deadline + d, where d is that source's current penalty.
// Then it sleeps until its OWN deadline and is checked. N attempts that
// arrive together get deadlines t, t+d, t+2d, ..., so one source is capped
// at 1/d checks a second whatever its concurrency, and nobody is turned
// away -- they wait their turn.
//
// Two earlier designs failed here, and both failures are worth keeping
// written down.
//
// A LOCKOUT after N failures, keyed by source, is a denial of service
// anyone can trigger. docker-proxy is a plain TCP relay, so behind the
// Dockerfile and compose file this repo ships, every request arrives from
// one address and the attacker's key IS the operator's. Five wrong guesses
// from a passer-by stopped the operator's correct password from working.
//
// A PER-REQUEST DELAY, with attempts past a per-key sleeping limit checked
// immediately so that nothing is refused, meters nothing at all. The
// overflow condition is created by the attacker: they park enough requests
// to fill their own sleeping slots and every further guess skips the delay.
// Measured, that was 0.5 checks a second at one worker and 8,399 at eight.
// The four sleepers were the attacker's own doorstop. Worse, the flood drove
// the shared key's penalty to the cap while the flooder paid none of it, so
// the honest operator waited the full two seconds and the attacker waited
// 0.43ms: the control inverted rather than merely weakened.
//
// Queueing is what has neither hole. There is no overflow condition for an
// attacker to create, because there is no branch that skips the wait: the
// only way to be checked sooner is for the queue in front of you to be
// shorter, and every attempt lengthens it by exactly d.
//
// Measured rather than argued, because two rounds of reasoning about this
// were wrong and a twenty-line harness caught both. One source, wrong
// password, comparisons counted over a 30-second window
// (TestTheLoginRateIsFlatAcrossWorkerCounts):
//
//	workers=1      0.67 /sec
//	workers=4      0.77 /sec
//	workers=8      0.87 /sec
//	workers=64     1.43 /sec
//	workers=512    1.50 /sec
//
// Flat, converging on the 0.5/s the delay cap sets, against 8,399/sec at
// eight workers under the design this replaced.
//
// The residual slope from 0.67 to 1.50 is an opening burst, and it is worth
// naming rather than rounding away: a source with no entry yet carries no
// penalty, so every attempt that arrives before the first failure is
// recorded is checked at once. That window is one password comparison wide,
// and claims serialise on this mutex, so it is a few dozen guesses -- about
// 25 of the checks in the 512-worker run above -- once, and again only
// after the source has been idle for a whole throttleWindow. Charging a
// penalty before anybody has failed would close it at the cost of delaying
// every honest first sign-in, which is not a trade worth making for a few
// dozen guesses against 2^33.
//
// The one escape is the global sleeper guard below, and it is deliberately
// not per key, so nothing an attacker does to their own queue can open it.
//
// Only the login uses this. The claim token is 79.3 bits of crypto/rand,
// so brute forcing it is arithmetically impossible and metering it buys
// nothing -- while a delay on the claim route would hand a passer-by the
// ability to slow the very operation this flow protects. See serveClaim.
const (
	// throttleBaseDelay is the first failure's penalty, doubling with each
	// consecutive failure after it. It is also the spacing of the queue:
	// one check per throttleBaseDelay per source until the penalty grows.
	throttleBaseDelay = 100 * time.Millisecond

	// throttleMaxDelay caps the penalty, and so sets the floor on a
	// sustained attack: one source gets at most one check every two
	// seconds, 0.5 a second, 43,200 a day. That is the number the claim
	// screen's password minimum is sized against; see
	// claimPasswordMinLength, and change neither without the other.
	//
	// Two seconds rather than ten because it is also what an operator who
	// mistyped their password waits, and a page that looks hung is a page
	// somebody restarts.
	throttleMaxDelay = 2 * time.Second

	// throttleWindow is the entry's TTL, and so how long a queue position
	// and a penalty survive. A source that has not failed for this long is
	// forgotten entirely: its next attempt is immediate, at no penalty.
	// Without this a key flooded once would stay penalised forever.
	throttleWindow = 5 * time.Minute

	// throttleMax bounds the map.
	//
	// The map is keyed by the request's source, which is the one thing an
	// attacker chooses. Without a bound, a flood of distinct sources grows
	// it forever, and a fix for a brute-force hole would have bought a
	// memory-exhaustion one. So the table is capped, and an insert at the
	// cap evicts an existing entry at random.
	//
	// Eviction loses a key's queue position and penalty, which fails
	// toward LESS delay, so it is worth being precise about what it costs
	// an attacker to provoke. Eviction only happens on an insert, an
	// insert only happens for a source not already in the table, and each
	// one drops a uniformly random entry -- so shaking out one specific
	// entry takes on the order of throttleMax inserts from throttleMax
	// DISTINCT sources. An attacker who has that many distinct sources
	// already has that many independent budgets and has no need of the
	// trick; an attacker behind a shared hop, which is the case this whole
	// design is about, has exactly one key and cannot insert a second.
	//
	// Random and not oldest-first: finding the oldest means scanning the
	// whole table on every insert, under the one mutex that also
	// serialises every login, which is a better denial of service than the
	// one it guards against.
	throttleMax = 4096

	// throttleMaxSleepers bounds how many requests may be waiting across
	// all sources, as a guard on goroutines and nothing more. A waiting
	// request's connection and its net/http goroutine exist whether the
	// handler waits or not, so the marginal cost is one blocked goroutine;
	// 4096 of them is on the order of 30MB of stacks.
	//
	// An attempt that cannot get a slot is CHECKED, not refused -- nothing
	// here may ever turn a correct password away. That is an escape from
	// the queue, so it is worth saying exactly what it costs to reach.
	// Every slot is held by a request that is waiting, so an attacker must
	// keep more than 4096 requests in flight simultaneously to saturate it:
	// several thousand open sockets, sustained, which is an ordinary HTTP
	// flood rather than anything credential-shaped, and which is loud in
	// every way a credential attack is not. It also cannot be reached by
	// the cheap trick that broke the previous design, because the guard
	// counts every source together: an attacker cannot fill it with their
	// own key alone without also filling it against themselves.
	//
	// It has a second use. A flood that does saturate the pool relieves the
	// operator as well: their attempt is checked at once rather than
	// queued behind the attacker's, so the worst a small flood can do is
	// make them wait, and a large one cannot even do that.
	throttleMaxSleepers = 4096
)

type throttleEntry struct {
	// fails is consecutive failures, which sets the penalty.
	fails int
	// last is when the last failure was recorded, for the TTL.
	last time.Time
	// next is the head of this source's queue: the earliest moment the
	// next arriving attempt may be checked.
	next time.Time
}

type throttle struct {
	mu       sync.Mutex
	at       map[string]throttleEntry
	sleepers int

	// now is time.Now, and base is throttleBaseDelay, except in tests,
	// which need to step over the window and to not spend real seconds
	// waiting.
	now  func() time.Time
	base time.Duration
}

func newThrottle() *throttle {
	return &throttle{
		at:   make(map[string]throttleEntry),
		now:  time.Now,
		base: throttleBaseDelay,
	}
}

// throttleKey is the source a penalty is charged to: the /64 for an IPv6
// address, the whole address for IPv4.
//
// Not the full IPv6 address. A single interface is routinely handed a /64,
// so keying on the address would give one attacker 2^64 independent
// budgets -- and 2^64 table entries to push everyone else's out with. The
// /64 is the smallest unit an operator is actually allocated, so it is the
// smallest unit worth charging.
//
// The port is always dropped: it changes on every connection, so keying on
// it would mean every attempt looked like a first one. An address that
// will not parse is charged as itself, which is fail-closed in the only
// direction that matters here -- it can cost a delay, never skip one.
func throttleKey(remoteAddr string) string {
	host := hostOfAddr(remoteAddr)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// hostOfAddr is the address half of a "host:port" remote address. It
// returns the input unchanged when there is no port to strip.
//
// Two callers, and they want different things from it. throttleKey needs a
// stable identity for a source -- the port changes on every connection, so
// keeping it would make every attempt look like a first one -- and that
// makes this security-relevant rather than cosmetic. The claim log line
// just wants to name what was seen, and a refusal that names nothing is
// worse than one that names something imprecise, which is why this returns
// the input rather than an error.
func hostOfAddr(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

// penalty is what one attempt from a source with n consecutive failures
// adds to that source's queue: base, doubling per failure, to
// throttleMaxDelay.
func (t *throttle) penalty(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := t.base
	for i := 1; i < n && d < throttleMaxDelay; i++ {
		d *= 2
	}
	return min(d, throttleMaxDelay)
}

// delay is the penalty a source is currently carrying, which is the spacing
// of its queue. It is not how long the next attempt waits -- that depends
// on how many are already queued in front of it; see claim.
func (t *throttle) delay(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.at[key]
	if !ok {
		return 0
	}
	if t.now().Sub(e.last) >= throttleWindow {
		// Expired: drop it here as well as on insert, so a source that
		// waits out its failures costs nothing afterwards.
		delete(t.at, key)
		return 0
	}
	return t.penalty(e.fails)
}

// claim takes this attempt's place in key's queue and reports when it may
// be checked, or false when the source carries no penalty and may be
// checked at once.
//
// The read of the queue head and the write that moves it happen together,
// under one lock, which is what makes two simultaneous attempts get two
// different deadlines rather than the same one. A claim is not given back:
// an attempt that is cancelled, or that gets checked early because the
// sleeper pool was full, has still spent its slot. Otherwise abandoning
// requests would be a way to buy the rate back.
func (t *throttle) claim(key string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	e, ok := t.at[key]
	if !ok {
		return time.Time{}, false
	}
	if now.Sub(e.last) >= throttleWindow {
		delete(t.at, key)
		return time.Time{}, false
	}
	d := t.penalty(e.fails)
	if d <= 0 {
		return time.Time{}, false
	}

	deadline := e.next
	if deadline.Before(now) {
		deadline = now
	}
	e.next = deadline.Add(d)
	t.at[key] = e
	return deadline, true
}

// wait holds this attempt until its turn in key's queue.
//
// It returns an error ONLY when ctx is cancelled -- a client that gives up
// must not leave a goroutine waiting on its behalf. It never reports "too
// busy" and never refuses: an attempt that cannot get one of the global
// sleeping slots is checked immediately, having already spent its place in
// the queue.
func (t *throttle) wait(ctx context.Context, key string) error {
	deadline, queued := t.claim(key)
	if !queued {
		return nil
	}
	d := deadline.Sub(t.now())
	if d <= 0 {
		return nil
	}
	if !t.beginSleep() {
		return nil
	}
	defer t.endSleep()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// beginSleep claims one of the throttleMaxSleepers slots, reporting
// whether there was one to claim.
func (t *throttle) beginSleep() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sleepers >= throttleMaxSleepers {
		return false
	}
	t.sleepers++
	return true
}

// endSleep releases a slot. The guard is for an unpaired call that no
// current path makes: a counter driven negative would silently raise the
// cap, which is the one direction this must not fail.
func (t *throttle) endSleep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sleepers > 0 {
		t.sleepers--
	}
}

// fail records one failed attempt from key, which is what the penalty is
// computed from.
func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	e, ok := t.at[key]
	if ok && now.Sub(e.last) >= throttleWindow {
		// The previous failures are older than the window, so they are not
		// evidence about this one, and neither is the queue they built.
		e = throttleEntry{}
	}
	if !ok {
		t.makeRoom()
	}
	e.fails++
	e.last = now
	t.at[key] = e
}

// clear forgets key: its penalty and its queue. Called on every success,
// so an operator who mistyped twice and then got it right does not carry
// those two failures into their next sign-in.
func (t *throttle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.at, key)
}

// makeRoom evicts one entry at random if the table is full, so a new key
// can be inserted without the map growing. O(1): see throttleMax for why
// this does not go looking for the best entry to drop, and for what
// provoking an eviction costs. Caller holds t.mu.
func (t *throttle) makeRoom() {
	if len(t.at) < throttleMax {
		return
	}
	// Go randomises where a map range starts, so the first key out of a
	// fresh range is an arbitrary one. That is all the randomness this
	// needs, and it costs one iteration rather than a scan.
	for k := range t.at {
		delete(t.at, k)
		return
	}
}

// len is for tests, which need to see that the bound actually binds.
func (t *throttle) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.at)
}
