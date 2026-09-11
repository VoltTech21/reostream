package control

import (
	"testing"
	"time"
)

// TestFreshSessionIsAccepted is the baseline: a token just issued is valid
// immediately, before any expiry logic gets a chance to reject it wrongly.
func TestFreshSessionIsAccepted(t *testing.T) {
	s := newSessionStore()
	tok, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(tok) {
		t.Fatal("a session issued moments ago was rejected")
	}
}

// TestExpiredSessionIsRejected drives the store's clock past sessionTTL
// without sleeping: this is an absolute expiry from issue, with no renewal
// on access, so moving the clock forward is enough to make an old token dead
// even though nothing has "used" it in the meantime.
func TestExpiredSessionIsRejected(t *testing.T) {
	now := time.Now()
	s := newSessionStore()
	s.now = func() time.Time { return now }

	tok, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(tok) {
		t.Fatal("a session issued a moment ago should still be valid")
	}

	now = now.Add(sessionTTL + time.Second)
	if s.valid(tok) {
		t.Fatal("a session past its 12 hour absolute expiry was still accepted")
	}
}

// TestExpiredSessionsArePurgedFromTheMap covers the actual "map grows
// without bound" failure: an expired token must not merely fail valid, it
// must be removed, or a store that only ever gets asked about fresh tokens
// (an abandoned cookie nobody presents again) would keep every session it
// ever issued forever.
func TestExpiredSessionsArePurgedFromTheMap(t *testing.T) {
	now := time.Now()
	s := newSessionStore()
	s.now = func() time.Time { return now }

	stale, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(sessionTTL + time.Second)

	fresh, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}

	// issue itself sweeps, so by here stale should already be gone; valid
	// on fresh also sweeps, as a second path to the same guarantee.
	s.mu.Lock()
	_, staleStillPresent := s.tokens[stale]
	n := len(s.tokens)
	s.mu.Unlock()
	if staleStillPresent {
		t.Fatal("an expired session was not purged from the map")
	}
	if n != 1 {
		t.Fatalf("session map holds %d entries, want exactly the 1 live one", n)
	}
	if !s.valid(fresh) {
		t.Fatal("the fresh session issued after the sweep should still be valid")
	}
}

// TestLogStreamDoesNotRenewASession pins the policy that a long-lived
// /logs/stream connection is not evidence anyone is still there: valid must
// not slide the expiry forward just because it was called again.
func TestValidDoesNotRenewExpiry(t *testing.T) {
	now := time.Now()
	s := newSessionStore()
	s.now = func() time.Time { return now }

	tok, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}

	// Repeated checks, as a held-open /logs/stream connection would trigger
	// on every retry or reconnect attempt, must not push the expiry out.
	for i := 0; i < 5; i++ {
		if !s.valid(tok) {
			t.Fatal("session unexpectedly rejected before expiry")
		}
	}

	now = now.Add(sessionTTL + time.Second)
	if s.valid(tok) {
		t.Fatal("repeated valid() calls renewed the session past its absolute expiry")
	}
}
