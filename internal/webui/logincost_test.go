package webui

import (
	"testing"
	"time"
)

// The login has no rate limit, by design and after four of them were built
// and measured away (see serveLogin in internal/control). The cost of one
// check is therefore the entire bound on how fast the page can be guessed
// at, and claimPasswordMinLength's arithmetic is written against it.
//
// So the cost is a behavior worth a test. A refactor that cached the
// derived password, or dropped the iteration count to make a test suite
// faster, would silently return the login to ~8,400 guesses a second and
// leave a 12 character floor that was only ever justified by this.
func TestAWrongPasswordCostsRealTime(t *testing.T) {
	a := Auth{Password: "a password of some length"}

	start := time.Now()
	if a.Check("not the password") {
		t.Fatal("accepted a wrong password")
	}
	cost := time.Since(start)

	// Generous floor: the calibration measured ~166ms for two derivations
	// on the machine this was written on, and a slower or busier one only
	// makes it longer. Anything under 20ms means the work is gone, not that
	// the machine is fast.
	if cost < 20*time.Millisecond {
		t.Errorf("a password check took %v, which is too cheap to be deriving anything. "+
			"loginKDFIterations is %d; if that is still right then Check stopped using it.",
			cost, loginKDFIterations)
	}
}

// And the cost must not have been bought by breaking the check itself.
func TestCheckStillAcceptsAndRejects(t *testing.T) {
	a := Auth{Password: "correct horse battery staple"}
	if !a.Check("correct horse battery staple") {
		t.Error("rejected the correct password")
	}
	for _, wrong := range []string{
		"",
		"correct horse battery stapl",
		"correct horse battery staple ",
		"CORRECT HORSE BATTERY STAPLE",
	} {
		if a.Check(wrong) {
			t.Errorf("accepted %q", wrong)
		}
	}
}

// Two different passwords must not derive to the same key, which a careless
// xor loop would do for inputs that differ only past the first block.
func TestStretchSeparatesSimilarPasswords(t *testing.T) {
	// A small iteration count: this is about the construction, not the cost.
	a, _ := stretch("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab", 16)
	b, _ := stretch("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaac", 16)
	if a == b {
		t.Error("two passwords differing only in their last byte derived to the same key")
	}
	if empty, _ := stretch("", 16); a == empty {
		t.Error("a long password derived to the same key as an empty one")
	}
}
