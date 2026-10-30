// Package rtsp serves camera streams over RTSP.
//
// It exists for compatibility with software that takes an RTSP URL and
// nothing else. It is not a replacement for the HTTP MPEG-TS output, which
// is stateless, self-resynchronizing and the more forgiving of the two under
// a network blip. Both run.
package rtsp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

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
// The description is fixed when the server stream is initialized, so
// initializing on the first keyframe would lock audio out for the life of the
// stream. Waiting forever is equally wrong: a camera with no microphone would
// never become describable. Cameras that do send AAC interleave it with video
// continuously, so a frame or two is enough in practice and this is generous.
const audioProbeFrames = 30

// rtpClockHz is the RTP clock for H264 and H265. Both run at 90 kHz over RTP
// regardless of frame rate.
const rtpClockHz = 90000

// rtpTime converts a camera microsecond reading to RTP clock ticks without
// regard for the wrap, which is why nothing in the packetizing path may use
// it. It survives because it is the arithmetic ts.Clock performs internally
// and the tests pin it.
//
// The camera's Micros is a uint32 of microseconds, so it returns to zero
// every 2^32 microseconds, which is 4294.97 seconds or 71.58 minutes. Fed
// straight to RTP, every reader saw the timestamp fall by 386,547,056 ticks
// -- 4,295 seconds at 90 kHz -- on that cycle, and read it as a stall or a
// seek. Measured on the live fleet on 2026-10-16, Frigate logged "No frames
// received in 20 seconds" on one or two cameras every 71.5 minutes, and
// discarded the recording segment it was writing at the time: 69 lost
// segments a day, spread across all eight cameras because they had been
// powered on together and their clocks wrapped within seconds of each other.
//
// The MPEG-TS output never had this fault; ts.Clock has counted wraps since
// it was written. Only this path was raw, which is why the symptom followed
// RTSP readers -- the recorder and the cam wall -- and left the HTTP stream
// alone.
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
	// epoch90 is added to every RTP timestamp: the wall clock in 90 kHz
	// ticks at this stream's first video frame. Each run of the daemon starts its
	// timeline from zero, so without it a restart sent timestamps back to
	// where the last run began. go2rtc keeps a recorder (Frigate's)
	// attached across that restart and passes the jump straight on, and
	// ffmpeg cannot write time that runs backwards: it piled minutes of
	// video into one segment and wrote nothing new until the clock caught
	// up, or until Frigate's watchdog restarted it. From the wall clock, a
	// restart is a short gap forward instead.
	epoch90 uint64

	mu      sync.Mutex
	ready   bool
	readers int // attached readers, gating packetization

	srv   *gortsplib.Server
	ss    *gortsplib.ServerStream
	desc  *description.Session
	video *description.Media
	enc   videoEncoder

	// Held between the first keyframe and initialization, while waiting to
	// see whether this camera sends audio.
	pendingKey *baichuan.Frame
	probed     int

	audio      *description.Media
	audioEnc   audioEncoder
	audioCfg   *mpeg4audio.AudioSpecificConfig
	audioClock *ts.AudioClock
	videoClock *ts.Clock
	lastVideo  uint64 // last video PTS in 90 kHz ticks, for the audio clock

	// Set by Connected and cleared by the first video frame and the first
	// keyframe of the new connection respectively. See Connected.
	rebaseClock   bool
	checkParams   bool
	lastVideoWall time.Time // wall time of the last video frame, for the gap on rebase

	// now is the wall clock, replaceable by tests. Nil means time.Now.
	now func() time.Time

	packetized int

	// fps is the frame rate from the camera's last info frame, offered in
	// the description so ffprobe (and Frigate's camera probe) can read it.
	fps int

	// talker opens talk sessions on this stream's camera, and speaker is
	// whether the camera has anything to play them through. Both must be
	// settled before the first keyframe: the description is fixed once the
	// server stream is up, and a back channel is only offered when both
	// hold. See SetTalker and SetSpeaker.
	talker  Talker
	speaker bool
	back    *description.Media

	// The last real keyframe and when video last arrived, for Hold.
	holdKey     *baichuan.Frame
	realMicros  uint32
	realWall    time.Time
	held        uint32
	holdRunning bool
}

// SetTalker gives the stream a way to reach its camera's speaker.
func (s *Stream) SetTalker(t Talker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.talker = t
}

// SetSpeaker records whether the camera has a speaker, from its own Support
// block. internal/stream calls it on each connection before asking for
// video, so it is known by the time the first keyframe builds the
// description.
func (s *Stream) SetSpeaker(has bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speaker = has
}

// backChannel returns the back channel media and the talker behind it, or
// nil when this stream offers none.
func (s *Stream) backChannel() (*description.Media, Talker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.back == nil {
		return nil, nil
	}
	return s.back, s.talker
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

// Connected implements stream.ConnectionSink. The stream.Run feeding this
// Stream calls it once per camera connection, before any frame from it.
//
// This session outlives the camera connection, so two things have to be
// re-examined when a new one starts. The timestamp clock: a rebooted camera
// counts microseconds from zero again, and a clock that is not told either
// freezes or jumps (ts.Clock.Resume has the measurements). And the
// parameter sets: the SDP carries the ones from the first keyframe, and a
// camera that comes back with different ones would leave every reader
// decoding against stale values, so the first keyframe of the new
// connection is compared and the description rebuilt only if they differ.
func (s *Stream) Connected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rebaseClock = true
	s.checkParams = true
}

// holdAfter is how long a camera may send no video before Hold repeats its
// last picture, and holdEvery how often it does. Readers such as Frigate's
// ffmpeg give up after about 20 seconds without a frame.
var (
	holdAfter = 3 * time.Second
	holdEvery = time.Second
)

// noteRealVideo records a frame that came from the camera, for Hold.
func (s *Stream) noteRealVideo(f baichuan.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.realMicros, s.realWall, s.held = f.Micros, s.wallNow(), 0
	if f.Kind == baichuan.FrameIFrame {
		k := f
		k.Data = append([]byte(nil), f.Data...)
		s.holdKey = &k
	}
}

// Hold keeps the stream showing its last picture while the camera sends no
// video, for a camera with hold_picture set (enabled reports whether it
// still is, so a config change takes effect without a restart). The last
// keyframe is repeated once a second, on the camera's own timeline carried
// forward, so the clock, and the reconnect rebase in videoFrame, see a
// stream that simply kept going. Live video takes over again as soon as it
// returns. It runs until ctx ends; a second call while one runs returns.
func (s *Stream) Hold(ctx context.Context, enabled func() bool) {
	s.mu.Lock()
	if s.holdRunning {
		s.mu.Unlock()
		return
	}
	s.holdRunning = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.holdRunning = false
		s.mu.Unlock()
	}()
	t := time.NewTicker(holdEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if f, ok := s.holdFrame(); ok && enabled() {
				s.videoFrame(f)
			}
		}
	}
}

// holdFrame returns the frame Hold should send now, if the camera has gone
// quiet long enough and there is a picture to hold.
func (s *Stream) holdFrame() (baichuan.Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holdKey == nil || s.wallNow().Sub(s.realWall) < holdAfter {
		return baichuan.Frame{}, false
	}
	s.held++
	f := *s.holdKey
	f.Micros = s.realMicros + s.held*uint32(holdEvery/time.Microsecond)
	return f, true
}

func (s *Stream) wallNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
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
		s.noteRealVideo(f)
		s.videoFrame(f)
	case baichuan.FrameAAC:
		s.aacFrame(f)
	case baichuan.FrameInfo:
		if f.FPS > 0 {
			s.mu.Lock()
			s.fps = f.FPS
			s.mu.Unlock()
		}
	default:
		// ADPCM has no RTP mapping worth carrying, and relabeling it as
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

	// After a reconnect, the first keyframe decides whether the description
	// still holds. Before the clock, so a rebuilt description and the
	// resumed timeline start together.
	s.refreshParameterSets(f)

	// Track the video clock whether or not anyone is watching. The audio
	// clock is anchored to it, and an anchor that only advances while a
	// reader is attached would start every new session's audio from
	// whenever the last one left.
	// Through a wrap-counting clock, never the raw camera reading: see
	// rtpTime. The 32 bit truncation on the way into the packet is the
	// ordinary RTP wrap, which happens every 13.25 hours at 90 kHz and is
	// what readers expect; what they cannot absorb is the clock going
	// backwards.
	s.mu.Lock()
	now := s.wallNow()
	if s.videoClock == nil {
		s.videoClock = ts.NewClock()
		// The clock's first frame is PTS zero, so the epoch is the wall
		// clock at that frame, not when the stream was registered: a camera
		// that took longer to answer this run than the last would otherwise
		// start the new timeline behind where the old one stopped.
		s.epoch90 = wallEpoch90(now)
	}
	if s.rebaseClock {
		// The gap is wall time since the last frame of the old connection,
		// which is the outage as a reader experienced it. For a reconnect
		// that was not a reboot the camera clock would have given the same
		// answer; for a reboot it is the only honest answer there is.
		s.videoClock.Resume(now.Sub(s.lastVideoWall))
		s.rebaseClock = false
	}
	s.lastVideoWall = now
	ts90 := s.videoClock.PTS(f.Micros)
	s.lastVideo = ts90
	epoch := s.epoch90
	s.mu.Unlock()

	if s.readerCount() == 0 {
		// Nobody is watching. The camera connection is held by the HTTP path
		// either way, so there is nothing to keep warm by packetizing into a
		// void.
		return
	}

	nalus := splitAnnexB(f.Video())
	if len(nalus) == 0 {
		return
	}

	s.mu.Lock()
	enc, ss, media := s.enc, s.ss, s.video
	s.packetized++
	s.mu.Unlock()
	if enc == nil || ss == nil {
		return
	}

	pkts, err := enc.Encode(nalus)
	if err != nil {
		return
	}
	ntp := ntpOf90(ts90 + epoch)
	for _, p := range pkts {
		p.Timestamp = uint32(ts90 + epoch)
		// A write error is a reader problem, not a camera problem. gortsplib
		// drops the reader; the camera goroutine carries on. The sender
		// report's wall time comes from the same timeline as the RTP
		// timestamp; see ntpOf90.
		_ = ss.WritePacketRTPWithNTP(media, p, ntp)
	}
}

// observeVideo advances the pre-initialization state machine: hold the first
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
		_ = s.initialize(*key)
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
		// initialized by its first audio frame is in this state for a few
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
	ts, ntp := s.audioRTPTime(f)
	for _, p := range pkts {
		p.Timestamp = ts
		_ = ss.WritePacketRTPWithNTP(media, p, ntp)
	}
}

// observeAudio records the AAC configuration from the first audio frame, so
// initialize can offer an audio track.
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
		_ = s.initialize(*key)
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
func (s *Stream) audioRTPTime(f baichuan.Frame) (uint32, time.Time) {
	s.mu.Lock()
	clock, lastVideo, cfg, epoch := s.audioClock, s.lastVideo, s.audioCfg, s.epoch90
	s.mu.Unlock()
	if clock == nil || cfg == nil {
		return 0, time.Now()
	}
	pts90 := clock.PTS(lastVideo, f.Data)
	return uint32((pts90 + epoch) * uint64(cfg.SampleRate) / rtpClockHz), ntpOf90(pts90 + epoch)
}

// ntpOf90 is the wall time a point on the RTP timeline stands for. The
// timeline is 90 kHz ticks since 1970 (wallEpoch90 plus the camera's own
// frame clock), and the RTCP sender report has to agree with it: stamped
// with the time a packet happened to be written instead, as WritePacketRTP
// does, a reader that maps RTP to wall time through the reports (ffmpeg,
// and so Frigate pointed straight at reostream) sees the mapping move with
// every report and its timestamps step back by up to 200 ms at keyframes
// (emulated Duo 3 and RLC-410, 2026-10-29).
func ntpOf90(t90 uint64) time.Time {
	return time.Unix(int64(t90/rtpClockHz), int64(t90%rtpClockHz)*1_000_000_000/rtpClockHz)
}

// initialize builds the description from the parameter sets carried in a
// keyframe and brings the server stream up.
func (s *Stream) initialize(f baichuan.Frame) error {
	fmtVideo, err := videoFormat(f)
	if err != nil {
		return err
	}

	media := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{fmtVideo},
	}
	medias := []*description.Media{media}

	// Audio joins here or not at all: the description is fixed once the
	// server stream is initialized.
	s.mu.Lock()
	audioCfg := s.audioCfg
	media.FrameRate = float64(s.fps)
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

	s.mu.Lock()
	var back *description.Media
	if s.talker != nil && s.speaker {
		back = backChannelMedia()
		medias = append(medias, back)
	}
	s.mu.Unlock()

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
	s.back = back
	if audioMedia != nil {
		s.audioClock = ts.NewAudioClock()
	}
	s.pendingKey = nil
	return nil
}

// videoFormat builds the RTP format, parameter sets included, from the
// elementary stream of a keyframe.
func videoFormat(f baichuan.Frame) (format.Format, error) {
	nalus := splitAnnexB(f.Video())

	// The camera reports its codec as "H265"/"H264". Compare case
	// insensitively rather than pinning the spelling: this string comes off
	// the wire and a model that varies it would silently serve nothing.
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
			return nil, errNoParameterSets
		}
		return &format.H265{PayloadTyp: 96, VPS: vps, SPS: sps, PPS: pps}, nil
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
			return nil, errNoParameterSets
		}
		return &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: sps, PPS: pps}, nil
	default:
		return nil, errUnsupportedCodec
	}
}

// sameParameterSets reports whether two video formats carry the same codec
// and the same parameter sets, which is what decides whether a description
// built from one still describes the other.
func sameParameterSets(a, b format.Format) bool {
	switch x := a.(type) {
	case *format.H265:
		y, ok := b.(*format.H265)
		return ok && bytes.Equal(x.VPS, y.VPS) && bytes.Equal(x.SPS, y.SPS) && bytes.Equal(x.PPS, y.PPS)
	case *format.H264:
		y, ok := b.(*format.H264)
		return ok && bytes.Equal(x.SPS, y.SPS) && bytes.Equal(x.PPS, y.PPS)
	}
	return false
}

// refreshParameterSets, on the first keyframe after Connected, compares the
// keyframe's parameter sets with the ones the description was built from
// and rebuilds the description if they differ. Rebuilding closes the server
// stream and so drops every reader, which is the right price for a stream
// that has actually changed and the wrong one for a camera that merely
// rebooted, which is why the comparison is made at all. A keyframe without
// usable parameter sets (a truncated one, see errNoParameterSets) decides
// nothing and the next keyframe is checked instead.
func (s *Stream) refreshParameterSets(f baichuan.Frame) {
	if f.Kind != baichuan.FrameIFrame {
		return
	}
	s.mu.Lock()
	pending, current := s.checkParams, s.video
	s.mu.Unlock()
	if !pending || current == nil {
		return
	}
	got, err := videoFormat(f)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.checkParams = false
	same := sameParameterSets(current.Formats[0], got)
	s.mu.Unlock()
	if same {
		return
	}
	s.close()
	s.mu.Lock()
	s.pendingKey = nil
	s.mu.Unlock()
	_ = s.initialize(f)
}

// errNoParameterSets means a keyframe arrived without the VPS, SPS or PPS the
// SDP needs. Reolink puts them inline in every keyframe, so this is a signal
// that the frame was truncated rather than a model that omits them, and
// waiting for the next keyframe is the right response.
var errNoParameterSets = errors.New("rtsp: keyframe carries no parameter sets")

// errUnsupportedCodec means the camera sent something neither H264 nor H265.
var errUnsupportedCodec = errors.New("rtsp: unsupported video codec")

// wallEpoch90 is t in 90 kHz ticks since the Unix epoch.
func wallEpoch90(t time.Time) uint64 {
	return uint64(t.Unix())*rtpClockHz + uint64(t.Nanosecond())*rtpClockHz/1_000_000_000
}
