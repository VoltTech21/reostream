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
