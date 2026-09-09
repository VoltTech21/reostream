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
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/pion/rtp"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/ts"
)

// audioProbeFrames is how many video frames to wait, after the first
// keyframe, for an audio frame to turn up before deciding the camera has no
// audio.
//
// The description is fixed when the server stream is initialised, so
// initialising on the first keyframe would lock audio out for the life of the
// stream. Waiting forever is equally wrong: a camera with no microphone would
// never become describable. Cameras that do send AAC interleave it with video
// continuously, so a frame or two is enough in practice and this is generous.
const audioProbeFrames = 30

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

	// Held between the first keyframe and initialisation, while waiting to
	// see whether this camera sends audio.
	pendingKey *baichuan.Frame
	probed     int

	audio      *description.Media
	audioEnc   audioEncoder
	audioCfg   *mpeg4audio.AudioSpecificConfig
	audioClock *ts.AudioClock
	lastVideo  uint64 // last video PTS in 90 kHz ticks, for the audio clock

	packetised int
}

// audioEncoder is the shape of the AAC RTP encoder this package uses.
type audioEncoder interface {
	Encode(aus [][]byte) ([]*rtp.Packet, error)
}

// description returns the session description, or nil before the stream is
// ready.
func (s *Stream) description() *description.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desc
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
		s.videoFrame(f)
	case baichuan.FrameAAC:
		s.aacFrame(f)
	default:
		// ADPCM has no RTP mapping worth carrying, and relabelling it as
		// something else would hand a client bytes it cannot decode.
	}
}

func (s *Stream) videoFrame(f baichuan.Frame) {
	if !s.Ready() {
		s.observeVideo(f)
		if !s.Ready() {
			return
		}
	}

	// Track the video clock whether or not anyone is watching. The audio
	// clock is anchored to it, and an anchor that only advances while a
	// reader is attached would start every new session's audio from
	// whenever the last one left.
	ts90 := uint64(rtpTime(f.Micros))
	s.mu.Lock()
	s.lastVideo = ts90
	s.mu.Unlock()

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
	for _, p := range pkts {
		p.Timestamp = uint32(ts90)
		// A write error is a reader problem, not a camera problem. gortsplib
		// drops the reader; the camera goroutine carries on.
		_ = ss.WritePacketRTP(media, p)
	}
}

// observeVideo advances the pre-initialisation state machine: hold the first
// keyframe, then wait a little to see whether audio turns up before fixing
// the description.
func (s *Stream) observeVideo(f baichuan.Frame) {
	s.mu.Lock()
	if s.pendingKey == nil {
		if f.Kind != baichuan.FrameIFrame {
			// Only a keyframe carries the parameter sets the SDP needs.
			s.mu.Unlock()
			return
		}
		key := f
		s.pendingKey = &key
	}
	s.probed++
	ready := s.audioCfg != nil || s.probed >= audioProbeFrames
	key := s.pendingKey
	s.mu.Unlock()

	if ready {
		_ = s.initialise(*key)
	}
}

// aacFrame carries one AAC access unit.
func (s *Stream) aacFrame(f baichuan.Frame) {
	if !s.Ready() {
		s.observeAudio(f)
		return
	}
	if s.readerCount() == 0 {
		return
	}

	s.mu.Lock()
	enc, ss, media, anchored := s.audioEnc, s.ss, s.audio, s.lastVideo != 0
	s.mu.Unlock()
	if enc == nil || ss == nil || media == nil {
		return
	}
	if !anchored {
		// No video PTS yet to anchor the audio clock to. Emitting now would
		// count samples from zero while video runs at the camera's clock,
		// which desyncs the tracks for the life of the session. A stream
		// initialised by its first audio frame is in this state for a few
		// milliseconds.
		return
	}

	aus, err := stripADTS(f.Data)
	if err != nil || len(aus) == 0 {
		return
	}
	pkts, err := enc.Encode(aus)
	if err != nil {
		return
	}
	ts := s.audioRTPTime(f)
	for _, p := range pkts {
		p.Timestamp = ts
		_ = ss.WritePacketRTP(media, p)
	}
}

// observeAudio records the AAC configuration from the first audio frame, so
// initialise can offer an audio track.
func (s *Stream) observeAudio(f baichuan.Frame) {
	s.mu.Lock()
	have, key := s.audioCfg != nil, s.pendingKey
	s.mu.Unlock()
	if have {
		return
	}

	cfg, err := adtsConfig(f.Data)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.audioCfg = cfg
	s.mu.Unlock()

	// A keyframe already seen means everything needed is now in hand.
	if key != nil {
		_ = s.initialise(*key)
	}
}

// audioRTPTime returns the RTP timestamp for one AAC frame.
//
// These cameras put no timestamp in an audio packet at all, so f.Micros is
// always zero and stamping from it would peg every audio packet to the start
// of the stream. The timestamp is reconstructed from the video clock and the
// samples emitted since, which is what the TS muxer already does; see
// ts.AudioClock and the incident recorded there.
//
// RTP for AAC runs at the sample rate, not at 90 kHz, so the reconstructed
// 90 kHz value is converted down.
func (s *Stream) audioRTPTime(f baichuan.Frame) uint32 {
	s.mu.Lock()
	clock, lastVideo, cfg := s.audioClock, s.lastVideo, s.audioCfg
	s.mu.Unlock()
	if clock == nil || cfg == nil {
		return 0
	}
	pts90 := clock.PTS(lastVideo, f.Data)
	return uint32(pts90 * uint64(cfg.SampleRate) / rtpClockHz)
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
	medias := []*description.Media{media}

	// Audio joins here or not at all: the description is fixed once the
	// server stream is initialised.
	s.mu.Lock()
	audioCfg := s.audioCfg
	s.mu.Unlock()

	var audioMedia *description.Media
	var audioEnc audioEncoder
	if audioCfg != nil {
		fmtAudio := &format.MPEG4Audio{
			PayloadTyp:       97,
			Config:           audioCfg,
			SizeLength:       13,
			IndexLength:      3,
			IndexDeltaLength: 3,
		}
		e, err := fmtAudio.CreateEncoder()
		if err != nil {
			// Video without audio beats no stream at all.
			audioCfg = nil
		} else {
			audioEnc = e
			audioMedia = &description.Media{
				Type:    description.MediaTypeAudio,
				Formats: []format.Format{fmtAudio},
			}
			medias = append(medias, audioMedia)
		}
	}

	desc := &description.Session{Medias: medias}

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
	s.audio, s.audioEnc = audioMedia, audioEnc
	if audioMedia != nil {
		s.audioClock = ts.NewAudioClock()
	}
	s.pendingKey = nil
	return nil
}

// errNoParameterSets means a keyframe arrived without the VPS, SPS or PPS the
// SDP needs. Reolink puts them inline in every keyframe, so this is a signal
// that the frame was truncated rather than a model that omits them, and
// waiting for the next keyframe is the right response.
var errNoParameterSets = errors.New("rtsp: keyframe carries no parameter sets")

// errUnsupportedCodec means the camera sent something neither H264 nor H265.
var errUnsupportedCodec = errors.New("rtsp: unsupported video codec")
