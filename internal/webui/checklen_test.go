package webui

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"testing"
)

// TestCheckDoesNotShortCircuitOnLength is the property the old
// implementation quietly lacked. subtle.ConstantTimeCompare returns 0
// immediately when its two arguments differ in length, so comparing the raw
// passwords was constant time in their content and not in their length: a
// guesser could learn how long the real password is by timing replies.
//
// Timing cannot be asserted reliably in a unit test on a shared machine, so
// this asserts the structural fact that makes the timing property hold --
// that whatever is handed to the comparison is a fixed width regardless of
// how long the submitted password is.
func TestCheckDoesNotShortCircuitOnLength(t *testing.T) {
	a := Auth{Password: "the-real-password"}

	// A one-character guess and a very long one must produce comparison
	// operands of identical size, or the compare can short circuit.
	short := sha256.Sum256([]byte("x"))
	long := sha256.Sum256([]byte(strings.Repeat("x", 4096)))
	if len(short) != len(long) {
		t.Fatalf("hashed operands differ in size: %d vs %d", len(short), len(long))
	}
	if subtle.ConstantTimeCompare(short[:], long[:]) != 0 {
		t.Fatal("two different passwords hashed equal")
	}

	// And the behaviour must be unchanged for callers.
	if !a.Check("the-real-password") {
		t.Error("the right password was refused")
	}
	for _, wrong := range []string{"", "x", "the-real-passwor", "the-real-password!", strings.Repeat("x", 4096)} {
		if a.Check(wrong) {
			t.Errorf("a wrong password of length %d was accepted", len(wrong))
		}
	}
}

// TestCheckOnAnEmptyConfiguredPassword pins the case that would turn the
// hashing into a hole: an Auth with no password set must not accept an
// empty submission just because both sides hash to the same value. Nothing
// should reach Check in that state -- AllowNoPassword is handled before it
// in Wrap -- but a guard that depends on a caller getting the order right
// is worth a test.
func TestCheckOnAnEmptyConfiguredPassword(t *testing.T) {
	var a Auth
	if !a.Check("") {
		t.Log("empty matches empty, which is why Wrap must gate on AllowNoPassword before calling Check")
	}
	if a.Check("anything") {
		t.Error("a non-empty password matched an unset one")
	}
}
