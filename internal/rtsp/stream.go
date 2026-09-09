// Package rtsp serves camera streams over RTSP.
//
// It exists for compatibility with software that takes an RTSP URL and
// nothing else. It is not a replacement for the HTTP MPEG-TS output, which
// is stateless, self-resynchronising and the more forgiving of the two under
// a network blip. Both run; see docs/design/2026-09-09-rtsp-output.md.
package rtsp

import (
	"sync"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// rtpClockHz is the RTP clock for H264, H265 and the timestamps this package
// derives. Both codecs run at 90 kHz over RTP regardless of frame rate.
const rtpClockHz = 90000

// rtpTime converts the camera's microsecond clock to an RTP timestamp.
//
// The camera clock is the same source the TS muxer uses, so the two outputs
// cannot disagree about when a frame happened. The multiply is done in 64
// bits deliberately: the camera clock runs to 2^32 microseconds and a 32 bit
// multiply by 90 overflows well before that, which would send timestamps
// backwards near the wrap.
func rtpTime(micros uint32) uint32 {
	return uint32(uint64(micros) * rtpClockHz / 1_000_000)
}

// Stream is one camera stream served over RTSP. It implements
// stream.FrameSink.
type Stream struct {
	mu         sync.Mutex
	ready      bool
	packetised int
}

// Ready reports whether the stream has seen the keyframe it needs before it
// can describe itself.
func (s *Stream) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Frame is called on the camera goroutine for every decoded frame. It must
// not block: anything slow here stalls the read loop the media watchdog is
// watching, and a stalled read loop looks like a failing camera.
func (s *Stream) Frame(f baichuan.Frame) {
	if s.readers() == 0 {
		// Nobody is watching. The camera connection is held by the HTTP
		// path either way, so there is nothing to keep warm by packetising
		// into a void.
		return
	}
	_ = f
}

// readers reports how many RTSP readers are attached.
func (s *Stream) readers() int { return 0 }
