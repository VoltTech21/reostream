package control

import (
	"fmt"
	"sync"
	"time"
)

// throttle is the shared lockout behind the two routes anyone who can reach
// this port may submit to without a session: the claim token, and the
// login password.
//
// Both are now brute-forceable by definition -- the claim gate no longer
// looks at where a request came from, and there is deliberately no
// password-strength rule to lean on -- so repeated failures from one source
// have to start costing something. After throttleFailures failures the
// source is refused outright for throttleWindow; one success clears the
// count, so an operator who mistypes twice and then gets it right is never
// held against.
//
// Failures only. A success must not create or extend an entry, or a busy
// page would throttle itself.
const (
	// throttleFailures is how many failures a source gets before it is
	// locked out. High enough to survive a fat-fingered token, low enough
	// that 79 bits of token stay 79 bits.
	throttleFailures = 5

	// throttleWindow is both the lockout length and the entry's TTL: a
	// source that has been quiet this long is forgotten entirely.
	throttleWindow = 5 * time.Minute

	// throttleMax bounds the map.
	//
	// The map is keyed by the request's source address, which is the one
	// thing an attacker chooses. Without a bound, a flood of spoofed or
	// merely numerous sources -- trivial over IPv6, where one attacker
	// holds a /64 -- grows it forever, and the fix for a brute-force hole
	// would have opened a memory-exhaustion one. So the table is capped:
	// expired entries are swept on every insert, and if the cap is still
	// reached the oldest entry is evicted to make room.
	//
	// Evicting the oldest is the right direction to lose accuracy in. The
	// oldest entry is the least recently failing source, so an attacker
	// who floods the table to push their own entry out has to keep failing
	// faster than everyone else they are displacing -- and displacing a
	// real operator's entry only ever gives that operator their attempts
	// back, never gives the attacker a password.
	throttleMax = 4096
)

type throttleEntry struct {
	fails int
	last  time.Time
}

type throttle struct {
	mu sync.Mutex
	at map[string]throttleEntry

	// now is time.Now except in tests, which need to step over the window
	// without sleeping through it.
	now func() time.Time
}

func newThrottle() *throttle {
	return &throttle{at: make(map[string]throttleEntry), now: time.Now}
}

// blocked reports whether key is currently locked out, and how long is left
// of it. The remaining time is for telling an operator when to try again;
// it says nothing an attacker does not already know.
func (t *throttle) blocked(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.at[key]
	if !ok {
		return 0, false
	}
	left := throttleWindow - t.now().Sub(e.last)
	if left <= 0 {
		// Expired: drop it here as well as in the sweep, so a source that
		// waits out its lockout costs nothing afterwards.
		delete(t.at, key)
		return 0, false
	}
	if e.fails < throttleFailures {
		return 0, false
	}
	return left, true
}

// fail records one failed attempt from key.
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
		t.makeRoom(now)
	}
	e.fails++
	e.last = now
	t.at[key] = e
}

// clear forgets key. Called on every success, which is what keeps an
// operator who mistyped a few times from being locked out by their own
// eventual success.
func (t *throttle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.at, key)
}

// makeRoom keeps the map at throttleMax entries or fewer before a new key
// is inserted: first by sweeping everything past its TTL, then, only if
// that was not enough, by evicting the single oldest entry. Caller holds
// t.mu.
//
// The sweep is O(n) over a map that is capped at throttleMax, and it only
// runs on a failed attempt for a source not already in the table, so the
// work an attacker can provoke is bounded by the cap rather than by how
// many distinct sources they can invent.
func (t *throttle) makeRoom(now time.Time) {
	if len(t.at) < throttleMax {
		return
	}
	for k, e := range t.at {
		if now.Sub(e.last) >= throttleWindow {
			delete(t.at, k)
		}
	}
	for len(t.at) >= throttleMax {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range t.at {
			if first || e.last.Before(oldest) {
				oldestKey, oldest, first = k, e.last, false
			}
		}
		if first {
			return
		}
		delete(t.at, oldestKey)
	}
}

// len is for tests, which need to see that the bound actually binds.
func (t *throttle) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.at)
}

// tooManyMessage is what a locked-out source reads. It says how long is
// left, rounded up to a whole second, because "try again later" with no
// number is the kind of answer that makes a person start restarting
// things.
func tooManyMessage(left time.Duration) string {
	secs := int((left + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	unit := "seconds"
	n := secs
	if secs >= 60 {
		n = (secs + 59) / 60
		unit = "minutes"
		if n == 1 {
			unit = "minute"
		}
	} else if secs == 1 {
		unit = "second"
	}
	return fmt.Sprintf("Too many failed attempts from this address. Wait %d %s and try again.", n, unit)
}
