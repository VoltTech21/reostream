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
//
// One Clock carries exactly one timestamped elementary stream. Its wrap
// detector is a single backward-step test against the previous value it was
// handed, so feeding it two sources of timestamps makes every alternation
// between them look like a step backwards. Video is the only stream these
// cameras timestamp at all (see AudioClock), so in this package that means
// video frames and nothing else. This is not a style preference: a build
// that fed audio frames through here counted a false wrap on every audio
// frame once the camera's uptime passed wrapThreshold, and each false wrap
// added 2^32 microseconds to every PTS after it. Measured against a live
// camera, that was 2,265 ffmpeg timestamp discontinuities in 60 seconds.
type Clock struct {
	started bool
	base    uint32 // camera time of the first frame
	last    uint32 // previous raw camera time, for wrap detection
	wraps   uint64 // how many times the camera clock has rolled over
	lastPTS uint64 // previous return value, for the monotonic clamp below
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

	// Total elapsed camera microseconds since the first frame. This has to be
	// signed: wrap detection above only fires on a backward step bigger than
	// wrapThreshold, so a smaller backward step (reordering, or the camera
	// clock stuttering) reaches here with micros < base and no wrap counted.
	// Computed as unsigned, that underflows to about 2^64 and truncates to a
	// garbage PTS; computed as signed, it comes out negative and is caught by
	// the clamp below instead.
	elapsed := int64(c.wraps<<32) + int64(micros) - int64(c.base)

	var pts uint64
	if elapsed > 0 {
		// Microseconds to 90 kHz ticks: multiply by 9, divide by 100. Done in
		// this order so the division does not throw away resolution.
		pts = uint64(elapsed) * 9 / 100
	}

	if pts < c.lastPTS {
		// A repeated timestamp reads to a decoder as a duplicate frame, which
		// is harmless. A timestamp that jumps backwards or forwards reads as
		// a stall or a seek, which is what an unnoticed underflow or a stray
		// out of order frame produces if this is emitted uncapped.
		return c.lastPTS
	}
	c.lastPTS = pts
	return pts
}

// Wraps reports how many times this clock has decided the camera's
// microsecond counter rolled over. On a stream shorter than 71.6 minutes it
// must be zero, which is what makes it a usable assertion in tests: a
// nonzero count on a short stream means the wrap detector fired on
// something that was not a wrap.
func (c *Clock) Wraps() uint64 { return c.wraps }

// LastPTS is the most recent timestamp this clock emitted, which is the
// stream's current position on the presentation timeline. AudioClock uses it
// to place audio frames, which arrive with no timestamp of their own.
func (c *Clock) LastPTS() uint64 { return c.lastPTS }

// Audio arrives from these cameras with no timestamp whatsoever. The media
// packet header for AAC and ADPCM is 8 bytes, magic(4) size(2) size(2), with
// no field for a capture time anywhere in it, unlike the 24 and 32 byte
// video headers which carry microseconds at offset 16. So an audio frame's
// Micros is not a different epoch from video's, it is the zero value, and
// there is nothing on the wire to recover it from. Its position in time has
// to be reconstructed here.
//
// The reconstruction is the audio's own sample clock. Every AAC frame these
// cameras emit is ADTS, and an ADTS header states the sample rate and how
// many raw data blocks the frame holds, so the frame's duration in samples
// is exact and free. Counting samples from an anchor taken off the video
// timeline gives audio a PTS that advances at the audio's real rate while
// starting in step with the picture. Observed on the committed capture:
// 16 kHz mono AAC-LC, one 1024 sample block per frame, so 64ms and 5760
// ticks per frame; 56 audio frames span 3.584s against 89 video frames at
// about 24.9fps spanning 3.57s, which is how the rate was confirmed rather
// than assumed.
//
// Anchoring to video rather than free-running from zero is the whole point.
// Two independent clocks each rebased to their own first frame would remove
// the false wraps and silently destroy lip sync, which is a worse bug: it is
// invisible until a recording is reviewed weeks later.
type AudioClock struct {
	started      bool
	anchor       uint64 // video PTS this run of audio frames counts from
	samples      uint64 // audio samples emitted since anchor
	rate         int    // sample rate those samples are counted at
	frameSamples uint64 // samples in the most recent frame
	lastPTS      uint64
}

func NewAudioClock() *AudioClock { return &AudioClock{} }

// audioResyncTicks is how far audio may drift from the video timeline before
// it is re-anchored: 1 second. Audio and video arrive interleaved on one TCP
// connection, so in normal running they are within a frame interval of each
// other and this never fires. A whole second of separation means audio
// frames were lost somewhere, and continuing to count samples from the old
// anchor would hold that gap forever.
const audioResyncTicks = 90000

// defaultAudioRate and defaultAudioFrameSamples are the fallback used for an
// AAC frame whose ADTS header will not parse, so a malformed frame costs one
// frame's worth of timing accuracy rather than being dropped or stalling the
// track. They are what every camera observed so far actually sends.
const (
	defaultAudioRate         = 16000
	defaultAudioFrameSamples = 1024
)

// PTS returns the presentation timestamp for one AAC frame, given the video
// stream's current position (Clock.LastPTS) and the frame's ADTS bytes.
func (a *AudioClock) PTS(videoPTS uint64, adts []byte) uint64 {
	samples, rate, ok := adtsFrame(adts)
	if !ok {
		samples = defaultAudioFrameSamples
		rate = a.rate
		if rate == 0 {
			rate = defaultAudioRate
		}
	}
	if !a.started || rate != a.rate {
		a.rate = rate
		a.reanchor(videoPTS)
		a.started = true
	}

	// Computed from the accumulated sample count rather than by adding a
	// per-frame tick duration, so the truncation in the 90kHz conversion
	// stays under one tick forever instead of accumulating. It matters for
	// any rate that does not divide 90000 evenly, such as 44100.
	pts := a.anchor + a.samples*90000/uint64(a.rate)
	if diffTicks(pts, videoPTS) > audioResyncTicks {
		a.reanchor(videoPTS)
		pts = a.anchor
	}

	a.samples += samples
	a.frameSamples = samples
	a.lastPTS = pts
	return pts
}

// reanchor restarts the sample count from a point on the video timeline,
// never letting audio step backwards: a PTS that goes backwards reads to a
// decoder as a seek, where a repeated one reads as a duplicate frame and is
// harmless.
func (a *AudioClock) reanchor(videoPTS uint64) {
	if a.started {
		if next := a.lastPTS + a.frameSamples*90000/uint64(a.rate); videoPTS < next {
			videoPTS = next
		}
	}
	a.anchor = videoPTS
	a.samples = 0
}

func diffTicks(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// adtsSampleRates is the sampling_frequency_index table from the ADTS
// header. Indices 13 to 15 are reserved and have no rate.
var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// adtsFrame reads the sample rate and frame duration out of an ADTS header.
//
// Every AAC profile these cameras can emit codes 1024 samples per raw data
// block, and the header's number_of_raw_data_blocks_in_frame field is a
// count minus one, so the frame holds (n+1) * 1024 samples.
func adtsFrame(b []byte) (samples uint64, rate int, ok bool) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
		return 0, 0, false
	}
	rate = adtsSampleRates[(b[2]>>2)&0x0F]
	if rate == 0 {
		return 0, 0, false
	}
	blocks := uint64(b[6]&0x03) + 1
	return blocks * 1024, rate, true
}
