package control

import (
	"context"
	"strings"
	"testing"
)

// TestBeginProbeRefusesASecondConcurrentClaim is the guard itself: two
// browser tabs, a double click, or a browser retry can each reach
// probeGuarded for the same unconfigured address before either has
// returned, and nothing about an HTTP handler in Go serialises that on its
// own.
func TestBeginProbeRefusesASecondConcurrentClaim(t *testing.T) {
	s := newTestServer(t, Options{})
	if !s.beginProbe("192.0.2.50:9000") {
		t.Fatal("first claim on an address should succeed")
	}
	if s.beginProbe("192.0.2.50:9000") {
		t.Fatal("a second concurrent claim on the same address should be refused")
	}
	s.endProbe("192.0.2.50:9000")
	if !s.beginProbe("192.0.2.50:9000") {
		t.Fatal("after endProbe releases the claim, the address should be claimable again")
	}
}

// TestBeginProbeDoesNotBlockADifferentAddress proves the guard is scoped
// per address, not a single global lock that would serialise unrelated
// cameras' setup probes.
func TestBeginProbeDoesNotBlockADifferentAddress(t *testing.T) {
	s := newTestServer(t, Options{})
	if !s.beginProbe("192.0.2.50:9000") {
		t.Fatal("first claim should succeed")
	}
	if !s.beginProbe("192.0.2.60:9000") {
		t.Fatal("an unrelated address must not be blocked by an in-flight probe of a different one")
	}
}

// TestProbeGuardedRefusesWhileAnotherProbeOfTheSameAddressIsInFlight drives
// probeGuarded itself, with the in-flight claim already held the way a
// real concurrent request would leave it, and checks the message a person
// sees names the actual reason rather than looking like any other failure.
func TestProbeGuardedRefusesWhileAnotherProbeOfTheSameAddressIsInFlight(t *testing.T) {
	s := newTestServer(t, Options{})
	s.inFlightProbes["203.0.113.5:9000"] = true

	rep := s.probeGuarded(context.Background(), "203.0.113.5", "admin", "x")
	if !strings.Contains(rep.Err, "already running") {
		t.Fatalf("got %+v, want an already-running refusal", rep)
	}
}
