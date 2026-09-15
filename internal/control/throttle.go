package control

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// throttle makes guessing the login password expensive without ever
// refusing the operator.
//
// It is a DELAY, not a lockout, and that distinction is the whole design.
// A lockout keyed by source looks right until you notice what RemoteAddr
// is in this product's own deployment: docker-proxy is a plain TCP relay,
// so behind the shipped Dockerfile and compose file every request arrives
// from 172.17.0.1 and the table has exactly one key for everybody. A
// refusal keyed like that is a denial of service anyone can trigger --
// five wrong guesses from a passer-by and the operator's CORRECT password
// stops working for minutes, repeatable forever. On an unclaimed install
// it would be worse in kind: somebody who cannot claim the install could
// still stop its owner from claiming it, which is the one operation this
// whole flow exists to protect.
//
// A delay has no such failure mode. A wrong guess costs the guesser the
// delay; a correct credential is NEVER refused, only made to wait, so an
// attacker sharing a hop with the operator can waste their seconds and
// nothing more. There is no exception to that, and there must not be: if
// any path here can turn a correct password away, the shared-hop attacker
// is back in business by finding it.
//
// The delay is applied BEFORE the credential is checked, from the failures
// already recorded. Checking first and only then consulting the throttle
// would remove the protection entirely: a wrong guess would be compared,
// recorded, and immediately retried at full speed.
//
// What the delay is actually worth, stated honestly. Requests are
// concurrent and each sleeps on its own, and failures are recorded after
// the check, so N attempts launched together all wait the same delay and
// are all judged against the same failure count. A delay curve alone
// therefore meters a serial attacker only; against a concurrent one the
// rate is set by how many attempts may be sleeping at once. That is what
// throttleMaxSleepersPerKey is for, and it is the number that does the
// metering: at the cap, four sleepers finishing every two seconds is about
// two guesses a second from one key. Not zero, which is why the claim
// screen now requires a password long enough that two guesses a second is
// hopeless -- see validClaimPassword. The two together are the control;
// neither is on its own.
//
// Only the login uses this. The claim token is 79.3 bits of crypto/rand,
// so brute forcing it is arithmetically impossible and delaying it buys
// nothing -- while a delay on the claim route would hand a passer-by the
// ability to slow the very operation this flow protects. See serveClaim.
const (
	// throttleBaseDelay is the first failure's cost, doubling with each
	// consecutive failure after it.
	throttleBaseDelay = 100 * time.Millisecond

	// throttleMaxDelay caps it. A couple of seconds is long enough that
	// guessing is hopeless and short enough that an operator who mistyped
	// their password does not think the page has hung.
	throttleMaxDelay = 2 * time.Second

	// throttleWindow is the entry's TTL: a source that has not failed for
	// this long is forgotten, and its next attempt costs nothing.
	throttleWindow = 5 * time.Minute

	// throttleMax bounds the map.
	//
	// The map is keyed by the request's source, which is the one thing an
	// attacker chooses. Without a bound, a flood of distinct sources grows
	// it forever, and a fix for a brute-force hole would have bought a
	// memory-exhaustion one. So the table is capped, and an insert at the
	// cap evicts an existing entry.
	//
	// Eviction is random, and deliberately not "oldest": finding the
	// oldest means scanning the whole table on every insert, under the one
	// mutex that also serialises every login, which is a far better denial
	// of service than the one it was guarding against. Random eviction is
	// O(1) and loses nothing that matters -- an attacker who floods the
	// table to evict their own entry gets their delay reset, which is the
	// same thing they get by waiting out the window, and the operator whose
	// entry is evicted only ever gets a shorter wait.
	throttleMax = 4096

	// throttleMaxSleepersPerKey is how many attempts from ONE source may be
	// sleeping at once. This is the limit that actually meters a brute
	// force, because a delay on its own does not: nothing serialises
	// attempts, so a hundred concurrent guesses would otherwise all wait
	// the same two seconds and all be judged, turning a two-second delay
	// into fifty guesses a second.
	//
	// Four, and extras are NOT refused -- they skip the delay and are
	// checked immediately. Refusing them would be the shared-hop denial of
	// service all over again, since behind docker-proxy the attacker's key
	// is the operator's key. Skipping the delay for the overflow costs
	// nothing an attacker can use: their guess rate is already whatever
	// their concurrency allows, and the four sleeping slots keep the
	// average cost per attempt near the delay for as long as they keep the
	// pressure on.
	//
	// At the cap this is roughly two guesses a second from one source,
	// against a password the claim screen requires to be at least
	// claimPasswordMinLength characters. That is the pair that has to hold,
	// not either half.
	throttleMaxSleepersPerKey = 4

	// throttleMaxSleepers bounds how many requests may be sleeping across
	// all sources, as a guard on goroutines and nothing more. A sleeping
	// request's connection and its net/http goroutine exist whether the
	// handler sleeps or not, so the marginal cost of a sleeper is one
	// blocked goroutine for at most throttleMaxDelay.
	//
	// It is deliberately two orders of magnitude above the old value.
	// Saturating it now takes roughly two thousand requests a second, which
	// is an ordinary HTTP flood rather than anything credential-specific --
	// and saturating it costs an attacker nothing anyway, because an
	// attempt that cannot get a slot is CHECKED, never refused.
	throttleMaxSleepers = 4096
)

type throttleEntry struct {
	fails int
	last  time.Time
}

type throttle struct {
	mu       sync.Mutex
	at       map[string]throttleEntry
	sleepers int

	// perKey counts how many attempts are sleeping for each source right
	// now. It is keyed by an attacker-controlled value like at, but it
	// needs no cap of its own: an entry exists only while a request is
	// sleeping, it is deleted when the count reaches zero, and the number
	// of sleepers is already bounded by throttleMaxSleepers.
	perKey map[string]int

	// now is time.Now, and base is throttleBaseDelay, except in tests,
	// which need to step over the window and to not spend real seconds
	// asleep.
	now  func() time.Time
	base time.Duration
}

func newThrottle() *throttle {
	return &throttle{
		at:     make(map[string]throttleEntry),
		perKey: make(map[string]int),
		now:    time.Now,
		base:   throttleBaseDelay,
	}
}

// throttleKey is the source a delay is charged to: the /64 for an IPv6
// address, the whole address for IPv4.
//
// Not the full IPv6 address. A single interface is routinely handed a /64,
// so keying on the address would give one attacker 2^64 independent delay
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

// delay is what the next attempt from key must wait: nothing for a source
// with no recent failures, then base, doubling per consecutive failure, to
// throttleMaxDelay.
func (t *throttle) delay(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.at[key]
	if !ok {
		return 0
	}
	if t.now().Sub(e.last) >= throttleWindow {
		// Expired: drop it here as well as in the sweep, so a source that
		// waits out its failures costs nothing afterwards.
		delete(t.at, key)
		return 0
	}
	d := t.base
	for i := 1; i < e.fails && d < throttleMaxDelay; i++ {
		d *= 2
	}
	return min(d, throttleMaxDelay)
}

// wait applies key's delay. It returns an error ONLY when ctx is cancelled
// -- a client that gives up must not leave a goroutine sleeping on its
// behalf -- and in particular it never reports "too busy": an attempt that
// cannot get a sleeping slot skips the delay and is checked. Refusing it
// instead would put "a correct password can be turned away" back into a
// design whose whole point is that it cannot be.
func (t *throttle) wait(ctx context.Context, key string) error {
	d := t.delay(key)
	if d <= 0 {
		return nil
	}
	if !t.beginSleep(key) {
		// Either this source already has throttleMaxSleepersPerKey
		// attempts waiting, or every slot on the page is taken. Check this
		// one without the delay rather than turning it away.
		return nil
	}
	defer t.endSleep(key)

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// beginSleep claims a sleeping slot for key, reporting whether there was
// one: at most throttleMaxSleepersPerKey for this source, and at most
// throttleMaxSleepers across all of them.
func (t *throttle) beginSleep(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.perKey[key] >= throttleMaxSleepersPerKey {
		return false
	}
	if t.sleepers >= throttleMaxSleepers {
		return false
	}
	t.sleepers++
	t.perKey[key]++
	return true
}

func (t *throttle) endSleep(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sleepers--
	if n := t.perKey[key] - 1; n > 0 {
		t.perKey[key] = n
	} else {
		// Deleted rather than left at zero, so the table holds only
		// sources that are sleeping right now and cannot be grown by a
		// flood of one-shot keys.
		delete(t.perKey, key)
	}
}

// fail records one failed attempt from key, which is what the next
// attempt's delay is computed from.
func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	e, ok := t.at[key]
	if ok && now.Sub(e.last) >= throttleWindow {
		// The previous failures are older than the window, so they are not
		// evidence about this one.
		e = throttleEntry{}
	}
	if !ok {
		t.makeRoom()
	}
	e.fails++
	e.last = now
	t.at[key] = e
}

// clear forgets key. Called on every success, so an operator who mistyped
// twice and then got it right does not carry those two failures into their
// next sign-in.
func (t *throttle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.at, key)
}

// makeRoom evicts one entry at random if the table is full, so a new key
// can be inserted without the map growing. O(1): see throttleMax for why
// this does not go looking for the best entry to drop. Caller holds t.mu.
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
