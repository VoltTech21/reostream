package ts

import (
	"math/rand"
	"testing"
	"time"
)

// fakeWall is a wall clock a test moves by hand, read as each frame's
// arrival.
type fakeWall struct{ t time.Time }

func (w *fakeWall) now() time.Time { return w.t }

// TestClockFollowsArrivalThroughCameraClockJumps is #33: an RLC-823S2 at
// night sends about 13.4 frames a second but stamps each 40 ms after the
// last, falls behind real time, and jumps 5 to 6 s forward every 12 s or
// so to catch up. Measured on the issue: 1596 frames in 120 s, 9 jumps of
// 5.2 to 6.2 s. Camera time alone turned each jump into a hole that size.
func TestClockFollowsArrivalThroughCameraClockJumps(t *testing.T) {
	w := &fakeWall{t: time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)}
	c := NewClock()
	c.UseArrival(w.now)

	const interval = 74600 * time.Microsecond // 13.4 fps
	start := w.t
	micros := uint32(4_250_000_000) // wraps 45 s in, for good measure
	camElapsed := time.Duration(0)
	var last uint64
	var maxStep uint64
	lagAtFollow := int64(-1)
	frames := 1600
	for i := 0; i < frames; i++ {
		if i > 0 {
			w.t = w.t.Add(interval)
			step := 40 * time.Millisecond
			real := w.t.Sub(start)
			if real-camElapsed > 5500*time.Millisecond {
				// The catch-up jump: back in line with real time.
				step = real - camElapsed
			}
			camElapsed += step
			micros += uint32(step / time.Microsecond)
		}
		pts := c.PTS(micros)
		if lagAtFollow < 0 && c.Followed() > 0 {
			lagAtFollow = int64(uint64(w.t.Sub(start)/time.Microsecond)*9/100) - int64(pts)
		}
		if i > 0 {
			if pts <= last {
				t.Fatalf("frame %d: PTS %d did not move forward from %d", i, pts, last)
			}
			if d := pts - last; d > maxStep {
				maxStep = d
			}
		}
		last = pts
	}
	if maxStep > 45000 {
		t.Fatalf("largest PTS step %d ticks (%.2f s), want no step over 0.5 s", maxStep, float64(maxStep)/90000)
	}
	// From the first jump on, the timeline keeps real time: no holes, and
	// no running at double speed between jumps either. Before it there is
	// nothing to tell this camera from one that keeps time, so the first
	// 12 s play fast and the timeline stays that much behind.
	real := int64(uint64(w.t.Sub(start)/time.Microsecond) * 9 / 100)
	if lagAtFollow < 0 {
		t.Fatal("never followed arrival")
	}
	if lag := real - int64(last); lag-lagAtFollow > 9000 || lagAtFollow-lag > 9000 {
		t.Fatalf("timeline lag went from %.2f s at the first jump to %.2f s at the end", float64(lagAtFollow)/90000, float64(lag)/90000)
	}
	if c.Followed() != 1 {
		t.Fatalf("Followed = %d, want 1: it switches once and stays while the camera keeps misbehaving", c.Followed())
	}
	if c.Wraps() != 1 {
		t.Fatalf("Wraps = %d, want 1", c.Wraps())
	}
}

// steadyRun feeds a 25 fps camera whose clock keeps time, with arrival
// jitter, a 3 s network stall followed by the backlog in a burst, and, when
// skip is set, the camera then skipping 2.5 s of frames it could not send.
// It returns the PTS from a clock that knows arrival and from one that
// does not.
func steadyRun(t *testing.T, skip bool) (withArrival, cameraOnly []uint64, c *Clock) {
	t.Helper()
	w := &fakeWall{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	c = NewClock()
	c.UseArrival(w.now)
	plain := NewClock()
	rng := rand.New(rand.NewSource(33))

	micros := uint32(10_000_000)
	captured := w.t
	for i := 0; i < 3000; i++ {
		if i > 0 {
			micros += 40_000
			captured = captured.Add(40 * time.Millisecond)
		}
		at := captured.Add(time.Duration(rng.Intn(30_000)) * time.Microsecond)
		switch {
		case i >= 1000 && i < 1075:
			// Stalled for 3 s, then the backlog all at once.
			at = captured.Add(3*time.Second - time.Duration(i-1000)*40*time.Millisecond)
			if i == 1074 && skip {
				// The camera dropped what it could not send and
				// carries on 2.5 s later.
				micros += 2_500_000
				captured = captured.Add(2500 * time.Millisecond)
			}
		case i >= 1075 && skip:
			at = at.Add(-2500 * time.Millisecond)
		}
		if at.Before(w.t) {
			at = w.t
		}
		w.t = at
		withArrival = append(withArrival, c.PTS(micros))
		cameraOnly = append(cameraOnly, plain.PTS(micros))
	}
	return withArrival, cameraOnly, c
}

// TestClockKeepsCameraTimeForASteadyCamera: a camera whose clock keeps time
// gets exactly the PTS it always got, through jitter and a stall.
func TestClockKeepsCameraTimeForASteadyCamera(t *testing.T) {
	for _, skip := range []bool{false, true} {
		got, want, c := steadyRun(t, skip)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("skip=%v frame %d: PTS %d with arrival, %d from camera time", skip, i, got[i], want[i])
			}
		}
		if c.Followed() != 0 {
			t.Fatalf("skip=%v: Followed = %d, want 0", skip, c.Followed())
		}
	}
}

// TestClockGoesBackToCameraTimeWhenItKeepsTime: the same camera keeps
// proper time by day, so after following arrival through the night it goes
// back to its own timing, carrying on from where arrival left it.
func TestClockGoesBackToCameraTimeWhenItKeepsTime(t *testing.T) {
	w := &fakeWall{t: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
	c := NewClock()
	c.UseArrival(w.now)
	start := w.t
	micros := uint32(1_000_000)
	camElapsed := time.Duration(0)
	var last uint64
	feed := func(i int, interval, step time.Duration) uint64 {
		if i > 0 || last > 0 {
			w.t = w.t.Add(interval)
			real := w.t.Sub(start)
			if real-camElapsed > 5500*time.Millisecond {
				step = real - camElapsed
			}
			camElapsed += step
			micros += uint32(step / time.Microsecond)
		}
		pts := c.PTS(micros)
		if last > 0 && (pts <= last || pts-last > 45000) {
			t.Fatalf("PTS stepped from %d to %d", last, pts)
		}
		last = pts
		return pts
	}
	for i := 0; i < 900; i++ { // night: about 67 s
		feed(i, 74600*time.Microsecond, 40*time.Millisecond)
	}
	if !c.follow {
		t.Fatal("not following arrival after a night of jumps")
	}
	// Day: 25 fps, stamped right. The last night jump has already put the
	// camera back in line with real time.
	camElapsed = w.t.Sub(start)
	for i := 0; i < 1500; i++ {
		feed(1, 40*time.Millisecond, 40*time.Millisecond)
	}
	if c.follow {
		t.Fatal("still following arrival after 60 s of a camera keeping time")
	}
	// Back on camera time, a frame is exactly the camera's 40 ms.
	before := last
	if got := feed(1, 47*time.Millisecond, 40*time.Millisecond); got-before != 3600 {
		t.Fatalf("step on camera time = %d ticks, want 3600", got-before)
	}
}

// TestClockWithoutArrivalIsCameraTimeOnly: no wall clock, no following, so
// a muxer replaying frames faster than real time keeps their own timing.
func TestClockWithoutArrivalIsCameraTimeOnly(t *testing.T) {
	c := NewClock()
	c.PTS(0)
	c.PTS(40_000)
	if got := c.PTS(6_040_000); got != 6_040_000*9/100 {
		t.Fatalf("PTS = %d, want the camera's own %d", got, 6_040_000*9/100)
	}
	if c.Followed() != 0 {
		t.Fatalf("Followed = %d without arrival", c.Followed())
	}
}
