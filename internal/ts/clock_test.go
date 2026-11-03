package ts

import (
	"testing"
	"time"
)

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

func TestClockResumesAfterTheCameraRestarts(t *testing.T) {
	// A rebooted camera starts its clock over from a small value. Seen by
	// the wrap detector that is either a backward step too small to be a
	// wrap, which clamps every frame to the last PTS until the new clock
	// catches up (up to 35 minutes frozen), or a step big enough to be
	// counted as one, which throws the PTS forward by up to 71 minutes.
	// Neither is a timestamp a reader can follow. Resume tells the clock the
	// connection was re-established: the next reading is a new base and the
	// timeline carries on from where it was, plus the real length of the
	// outage, so a recorder sees the gap as a gap and not as a seek.
	c := NewClock()
	c.PTS(200_000_000)
	last := c.PTS(260_000_000) // 60s streamed: 5,400,000 ticks
	if last != 5_400_000 {
		t.Fatalf("PTS before restart = %d, want 5400000", last)
	}

	c.Resume(15 * time.Second)
	if got := c.PTS(20_000_000); got != last+15*90000 {
		t.Fatalf("first PTS after restart = %d, want %d (last + 15s)", got, last+15*90000)
	}
	if got := c.PTS(20_040_000); got != last+15*90000+3600 {
		t.Fatalf("40ms after restart PTS = %d, want %d", got, last+15*90000+3600)
	}
	if c.Wraps() != 0 {
		t.Fatalf("the restart was counted as %d wraps", c.Wraps())
	}
	if got := c.LastPTS(); got != last+15*90000+3600 {
		t.Fatalf("LastPTS = %d after restart, want %d", got, last+15*90000+3600)
	}
}

func TestClockResumeBeforeAnyFrameIsAFreshStart(t *testing.T) {
	// The connection hook fires before the first frame of the first
	// connection too. With no timeline yet there is nothing to continue
	// from, and the first frame must still map to zero.
	c := NewClock()
	c.Resume(3 * time.Second)
	if got := c.PTS(99_000_000); got != 0 {
		t.Fatalf("first frame PTS = %d, want 0", got)
	}
}

func TestClockResumeAfterAWrapKeepsTheTimeline(t *testing.T) {
	// A clock that has already counted a wrap must carry that elapsed time
	// into the resumed timeline rather than starting the sum over.
	c := NewClock()
	c.PTS(4_294_000_000)
	c.PTS(1_000_000) // wrapped: 1.967s elapsed
	before := c.LastPTS()
	c.Resume(time.Second)
	if got := c.PTS(5_000_000); got != before+90000 {
		t.Fatalf("PTS after wrap+resume = %d, want %d", got, before+90000)
	}
}

// A camera whose time steps back gets the previous timestamp until it
// catches up, and each such frame is counted (#25).
func TestClockCountsHeldFrames(t *testing.T) {
	c := NewClock()
	for _, us := range []uint32{1_000_000, 1_050_000, 1_100_000, 900_000, 950_000, 1_150_000} {
		c.PTS(us)
	}
	if got := c.Held(); got != 2 {
		t.Fatalf("held %d frames, want 2", got)
	}
}
