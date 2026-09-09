package rtsp

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Reolink sends parameter sets inline with every keyframe, and they have to
// go in the SDP, so a stream cannot describe itself until it has seen one. A
// DESCRIBE before that must find nothing rather than an SDP with no
// parameter sets in it.
func TestStreamNotReadyBeforeKeyframe(t *testing.T) {
	s := &Stream{}
	if s.Ready() {
		t.Fatal("stream reports ready with no keyframe seen")
	}
}

// The RTP timestamp comes from the camera clock, the same source the TS
// muxer uses, so the two outputs cannot disagree about when a frame
// happened. 90 kHz for both H264 and H265.
func TestRTPTimestampFromCameraClock(t *testing.T) {
	for _, tc := range []struct {
		micros uint32
		want   uint32
	}{
		{0, 0},
		{1000, 90},
		{1_000_000, 90_000},
		{33_366, 3002},
	} {
		if got := rtpTime(tc.micros); got != tc.want {
			t.Errorf("rtpTime(%d) = %d, want %d", tc.micros, got, tc.want)
		}
	}
}

// The camera clock wraps at 2^32 microseconds. The conversion must not
// overflow on the way, which it does if the multiply happens in 32 bits.
func TestRTPTimestampDoesNotOverflowIn32Bits(t *testing.T) {
	const nearWrap = uint32(4_294_000_000)
	got := rtpTime(nearWrap)
	want := uint32(uint64(nearWrap) * 90 / 1000)
	if got != want {
		t.Errorf("rtpTime(%d) = %d, want %d", nearWrap, got, want)
	}
}

// Packetising for nobody is the cost this design exists to avoid: the HTTP
// path muxes continuously so a new client can join instantly, and RTSP does
// not need that because the camera connection is held either way.
func TestNoPacketisationWithoutReaders(t *testing.T) {
	s := &Stream{}
	s.Frame(baichuan.Frame{Kind: baichuan.FrameIFrame, Codec: "h265"})
	if s.packetised != 0 {
		t.Errorf("packetised %d times with no readers", s.packetised)
	}
}
