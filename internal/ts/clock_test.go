package ts

import "testing"

func TestClockRebasesToZero(t *testing.T) {
	// The camera's clock starts wherever it likes. The first frame must map to
	// PTS 0 so the stream does not begin at an arbitrary offset.
	c := NewClock()
	if got := c.PTS(123456789); got != 0 {
		t.Fatalf("first frame PTS = %d, want 0", got)
	}
}

func TestClockConvertsMicrosecondsTo90kHz(t *testing.T) {
	c := NewClock()
	c.PTS(1000000) // rebase here, one second on the camera clock
	// One further second must advance PTS by exactly 90000 ticks.
	if got := c.PTS(2000000); got != 90000 {
		t.Fatalf("one second later PTS = %d, want 90000", got)
	}
	// 40ms, a frame at 25fps, is 3600 ticks.
	if got := c.PTS(2040000); got != 93600 {
		t.Fatalf("40ms later PTS = %d, want 93600", got)
	}
}

func TestClockIsMonotonicAcrossWraparound(t *testing.T) {
	// Micros is a uint32 of microseconds, so it wraps every 2^32 us, about
	// 71.6 minutes. A camera streaming longer than that wraps to zero, and a
	// muxer that does not notice emits a PTS that leaps backwards, which
	// players show as a stall or a seek.
	c := NewClock()
	c.PTS(0)
	before := c.PTS(^uint32(0) - 1000000) // one second before the wrap
	after := c.PTS(1000000)               // two seconds later, having wrapped
	if after <= before {
		t.Fatalf("PTS went backwards across the wrap: %d then %d", before, after)
	}
	gap := after - before
	// Two seconds of camera time is 180000 ticks. Allow a tick of slack for
	// the integer division, nothing more.
	if gap < 179999 || gap > 180001 {
		t.Fatalf("gap across wrap = %d ticks, want about 180000", gap)
	}
}

func TestClockClampsSmallBackwardSteps(t *testing.T) {
	// A backward step small enough not to look like a wrap (reordering, or
	// the camera clock stuttering) must not underflow the elapsed-time
	// subtraction. Computed as unsigned, base > micros with no wrap counted
	// wraps around to about 2^64 and truncates to a garbage PTS after the
	// 90kHz conversion; the fix clamps to the previous PTS instead.
	c := NewClock()
	c.PTS(5000000)         // rebase here
	high := c.PTS(5040000) // a normal 40ms step forward
	// One millisecond earlier than the base, not just the previous frame: far
	// too small to be mistaken for a wrap, which needs a jump of over 35
	// minutes, so wrap detection does not fire and the raw subtraction
	// underflows unless it is guarded.
	low := c.PTS(4999000)
	if low != high {
		t.Fatalf("PTS after a small backward step = %d, want it clamped to %d", low, high)
	}
	// A sane bound: this stream is a few seconds old, so anything anywhere
	// near the 90kHz clock's 26.5 hour wraparound is proof of an underflow,
	// not a real timestamp.
	const oneHourOfTicks = 90000 * 3600
	if low > oneHourOfTicks {
		t.Fatalf("PTS = %d, implausible for a stream a few seconds old", low)
	}
}

func TestClockHandlesRepeatedWraps(t *testing.T) {
	c := NewClock()
	c.PTS(0)
	var last uint64
	for i := 0; i < 5; i++ {
		// Step most of the way round the uint32 range each time.
		last = c.PTS(uint32(i) * 0xF0000000)
	}
	if last == 0 {
		t.Fatal("PTS did not advance across repeated wraps")
	}
}
