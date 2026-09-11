package webui

import (
	"testing"
	"time"
)

func TestSessionExpiresAbsolutelyAndIsSweptFromTheMap(t *testing.T) {
	s := NewSessionStore()
	now := time.Now()
	s.SetClock(func() time.Time { return now })

	tok, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tok) {
		t.Fatal("a fresh session was not valid")
	}

	now = now.Add(SessionTTL + time.Second)
	if s.Valid(tok) {
		t.Fatal("an expired session was accepted")
	}
	// Sweeping matters as much as rejecting: a token nobody presents again
	// would otherwise sit in the map forever.
	if n := s.len(); n != 0 {
		t.Fatalf("map holds %d entries after expiry, want 0", n)
	}
}

func TestTwoSessionsDiffer(t *testing.T) {
	s := NewSessionStore()
	a, _ := s.Issue()
	b, _ := s.Issue()
	if a == b {
		t.Fatal("two sessions got the same token")
	}
}

// TestValidDoesNotRenewExpiry pins the no-sliding-renewal guarantee: the
// TTL is absolute from issue, so a long-lived response such as a log stream
// that calls Valid repeatedly must not push a session's expiry out just by
// asking about it.
func TestValidDoesNotRenewExpiry(t *testing.T) {
	s := NewSessionStore()
	now := time.Now()
	s.SetClock(func() time.Time { return now })

	tok, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if !s.Valid(tok) {
			t.Fatal("session unexpectedly rejected before expiry")
		}
	}

	now = now.Add(SessionTTL + time.Second)
	if s.Valid(tok) {
		t.Fatal("repeated Valid() calls renewed the session past its absolute expiry")
	}
}

// TestSweepIsSelective guards against a blanket sweep: with one stale token
// and one fresh token in the map, expiry must purge only the stale one. A
// sweep that clears everything would log every operator out at once,
// intermittently, whenever any one session happened to expire.
func TestSweepIsSelective(t *testing.T) {
	s := NewSessionStore()
	now := time.Now()
	s.SetClock(func() time.Time { return now })

	stale, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(SessionTTL + time.Second)

	fresh, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}

	if s.Valid(stale) {
		t.Fatal("a stale session was still accepted")
	}
	if n := s.len(); n != 1 {
		t.Fatalf("map holds %d entries after sweep, want exactly the 1 live one", n)
	}
	if !s.Valid(fresh) {
		t.Fatal("a fresh session was purged by the sweep of an unrelated stale one")
	}
}
