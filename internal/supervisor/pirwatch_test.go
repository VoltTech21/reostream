package supervisor

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
)

func answer(ms int, asleep bool) bcudp.Info {
	return bcudp.Info{Addr: net.IPv4(192, 0, 2, 10), Asleep: asleep, Battery: -1, RTT: time.Duration(ms) * time.Millisecond}
}

// One fast answer is not a wake; a failed search breaks a run; three in a
// row, once armed, are.
func TestPIRWatchNeedsARunOfFastAnswers(t *testing.T) {
	var w pirWatch
	now := time.Now()
	see := func(ms int) wakeVerdict { return w.see(now, answer(ms, true), nil, false) }
	for i := 0; i < pirArmRun; i++ {
		see(90)
	}
	if see(5) != wakeNothing || see(5) != wakeNothing {
		t.Fatal("motion before the third fast answer")
	}
	w.see(now, bcudp.Info{}, errors.New("no answer"), false)
	if see(5) != wakeNothing || see(5) != wakeNothing {
		t.Fatal("a failed search did not break the run")
	}
	if see(5) != wakeMotion {
		t.Fatal("three fast answers in a row are not motion")
	}
	// Still the same wake, slow answers in it or not.
	see(204)
	see(204)
	for i := 0; i < 5; i++ {
		if see(4) != wakeNothing {
			t.Fatal("one wake was motion twice")
		}
	}
}

// Fast answers from a camera that is awake for us are not motion, and
// leave it unarmed until it has been asleep a while.
func TestPIRWatchOursIsNotMotion(t *testing.T) {
	var w pirWatch
	now := time.Now()
	for i := 0; i < pirArmRun; i++ {
		w.see(now, answer(90, true), nil, false)
	}
	for i := 0; i < 5; i++ {
		if v := w.see(now, answer(4, true), nil, true); v != wakeNothing {
			t.Fatalf("our own wake read as %v", v)
		}
	}
	w.see(now, answer(40, false), nil, true)
	for i := 0; i < 3; i++ {
		if v := w.see(now, answer(4, true), nil, false); v != wakeNothing {
			t.Fatalf("settling after our session read as %v", v)
		}
	}
}

// A camera whose answers never come back fast is too far away for PIR
// wakes to show, and that is said once.
func TestPIRWatchSaysOnceWhenThePathIsTooSlow(t *testing.T) {
	var w pirWatch
	now := time.Now()
	n := 0
	for i := 0; i < 3*pirSlowPathAfter; i++ {
		if w.see(now, answer(45+i%80, true), nil, false) == wakeSlowPath {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("said %d times", n)
	}
	var near pirWatch
	for i := 0; i < 2*pirSlowPathAfter; i++ {
		if near.see(now, answer(18+i%80, true), nil, false) == wakeSlowPath {
			t.Fatal("a camera on the LAN was called too far away")
		}
	}
}
