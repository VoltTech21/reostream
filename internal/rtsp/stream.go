// Package rtsp serves camera streams over RTSP.
//
// It exists for compatibility with software that takes an RTSP URL and
// nothing else. It is not a replacement for the HTTP MPEG-TS output, which
// is stateless, self-resynchronising and the more forgiving of the two under
// a network blip. Both run; see docs/design/2026-09-09-rtsp-output.md.
package rtsp

import (
	"errors"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// rtpClockHz is the RTP clock for H264 and H265. Both run at 90 kHz over RTP
// regardless of frame rate.
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

// videoEncoder is what rtph264 and rtph265 encoders have in common.
type videoEncoder interface {
	Encode(au [][]byte) ([]*rtp.Packet, error)
}

// Stream is one camera stream served over RTSP. It implements
// stream.FrameSink.
//
// It cannot describe itself until it has seen a keyframe: Reolink sends
// parameter sets inline in the elementary stream rather than out of band, and
// they belong in the SDP. Everything below stays nil until then.
type Stream struct {
	mu      sync.Mutex
	ready   bool
	readers int // attached readers, gating packetisation

	srv   *gortsplib.Server
	ss    *gortsplib.ServerStream
	desc  *description.Session
	video *description.Media
	enc   videoEncoder

	packetised int
}

// Ready reports whether the stream has seen the keyframe it needs before it
// can describe itself.
func (s *Stream) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *Stream) serverStream() *gortsplib.ServerStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ss
}

func (s *Stream) addReader() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers++
}

func (s *Stream) removeReader() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readers > 0 {
		s.readers--
	}
}

func (s *Stream) readerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readers
}

func (s *Stream) close() {
	s.mu.Lock()
	ss := s.ss
	s.ss = nil
	s.ready = false
	s.mu.Unlock()
	if ss != nil {
		ss.Close()
	}
}

// Frame is called on the camera goroutine for every decoded frame. It must
// not block: anything slow here stalls the read loop the media watchdog is
// watching, and a stalled read loop looks like a failing camera.
func (s *Stream) Frame(f baichuan.Frame) {
	switch f.Kind {
	case baichuan.FrameIFrame, baichuan.FramePFrame:
	default:
		// Audio is handled separately, and ADPCM has no RTP mapping worth
		// carrying.
		return
	}

	if !s.Ready() {
		// Only a keyframe carries the parameter sets the SDP needs, so a P
		// frame before the first keyframe has nothing to initialise from.
		if f.Kind != baichuan.FrameIFrame {
			return
		}
		if err := s.initialise(f); err != nil {
			return
		}
	}

	if s.readerCount() == 0 {
		// Nobody is watching. The camera connection is held by the HTTP path
		// either way, so there is nothing to keep warm by packetising into a
		// void.
		return
	}

	nalus := splitAnnexB(f.Video())
	if len(nalus) == 0 {
		return
	}

	s.mu.Lock()
	enc, ss, media := s.enc, s.ss, s.video
	s.packetised++
	s.mu.Unlock()
	if enc == nil || ss == nil {
		return
	}

	pkts, err := enc.Encode(nalus)
	if err != nil {
		return
	}
	ts := rtpTime(f.Micros)
	for _, p := range pkts {
		p.Timestamp = ts
		// A write error is a reader problem, not a camera problem. gortsplib
		// drops the reader; the camera goroutine carries on.
		_ = ss.WritePacketRTP(media, p)
	}
}

// initialise builds the description from the parameter sets carried in a
// keyframe and brings the server stream up.
func (s *Stream) initialise(f baichuan.Frame) error {
	nalus := splitAnnexB(f.Video())

	// The camera reports its codec as "H265"/"H264". Compare case
	// insensitively rather than pinning the spelling: this string comes off
	// the wire and a model that varies it would silently serve nothing.
	var fmtVideo format.Format
	switch strings.ToLower(f.Codec) {
	case "h265":
		var vps, sps, pps []byte
		for _, n := range nalus {
			switch h265NALUType(n) {
			case h265VPS:
				vps = n
			case h265SPS:
				sps = n
			case h265PPS:
				pps = n
			}
		}
		if vps == nil || sps == nil || pps == nil {
			return errNoParameterSets
		}
		fmtVideo = &format.H265{PayloadTyp: 96, VPS: vps, SPS: sps, PPS: pps}
	case "h264":
		var sps, pps []byte
		for _, n := range nalus {
			switch h264NALUType(n) {
			case h264SPS:
				sps = n
			case h264PPS:
				pps = n
			}
		}
		if sps == nil || pps == nil {
			return errNoParameterSets
		}
		fmtVideo = &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: sps, PPS: pps}
	default:
		return errUnsupportedCodec
	}

	media := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{fmtVideo},
	}
	desc := &description.Session{Medias: []*description.Media{media}}

	var enc videoEncoder
	switch v := fmtVideo.(type) {
	case *format.H265:
		e, err := v.CreateEncoder()
		if err != nil {
			return err
		}
		enc = e
	case *format.H264:
		e, err := v.CreateEncoder()
		if err != nil {
			return err
		}
		enc = e
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if s.srv != nil {
		ss := &gortsplib.ServerStream{Server: s.srv, Desc: desc}
		if err := ss.Initialize(); err != nil {
			return err
		}
		s.ss = ss
	}
	s.desc, s.video, s.enc, s.ready = desc, media, enc, true
	return nil
}

// errNoParameterSets means a keyframe arrived without the VPS, SPS or PPS the
// SDP needs. Reolink puts them inline in every keyframe, so this is a signal
// that the frame was truncated rather than a model that omits them, and
// waiting for the next keyframe is the right response.
var errNoParameterSets = errors.New("rtsp: keyframe carries no parameter sets")

// errUnsupportedCodec means the camera sent something neither H264 nor H265.
var errUnsupportedCodec = errors.New("rtsp: unsupported video codec")
