// Package ts muxes decoded camera frames into an MPEG-TS byte stream.
//
// It does no I/O. Frames go in, 188 byte transport packets come out, which
// makes the timing arithmetic here exhaustively testable. That matters more
// than it sounds: getting timestamps wrong is not a subtle degradation, it
// produces streams that report impossible frame rates and players that drop
// thousands of frames.
package ts

// Clock converts the camera's capture time into MPEG-TS presentation
// timestamps.
//
// Two things make this more than a multiplication. The camera's clock starts
// at an arbitrary value, so the first frame is rebased to zero. And Micros is
// a uint32 of microseconds, so it wraps roughly every 71.6 minutes; an
// unnoticed wrap makes PTS jump backwards mid stream.
type Clock struct {
	started bool
	base    uint32 // camera time of the first frame
	last    uint32 // previous raw camera time, for wrap detection
	wraps   uint64 // how many times the camera clock has rolled over
}

func NewClock() *Clock { return &Clock{} }

// wrapThreshold is how far backwards the camera clock may appear to move
// before it is treated as a wrap rather than as frames arriving out of order.
// Half the uint32 range is the usual choice and needs no tuning: real
// reordering is milliseconds, a wrap is 35 minutes.
const wrapThreshold = 1 << 31

// PTS returns the 90 kHz presentation timestamp for a frame captured at the
// given camera time.
func (c *Clock) PTS(micros uint32) uint64 {
	if !c.started {
		c.started = true
		c.base = micros
		c.last = micros
		return 0
	}
	if micros < c.last && c.last-micros > wrapThreshold {
		c.wraps++
	}
	c.last = micros

	// Total elapsed camera microseconds since the first frame.
	elapsed := c.wraps<<32 + uint64(micros) - uint64(c.base)

	// Microseconds to 90 kHz ticks: multiply by 9, divide by 100. Done in this
	// order so the division does not throw away resolution.
	return elapsed * 9 / 100
}
