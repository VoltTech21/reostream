package ts

import (
	"fmt"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// tableRepeatTicks is how often PAT and PMT are resent, in 90kHz ticks. A
// client that joins mid stream cannot decode anything until it has both
// tables, so they cannot only be sent once at the start. This is measured
// against the camera's own clock rather than a frame count so it holds at
// roughly the same wall-clock interval regardless of frame rate: 500ms is
// comfortably under the few seconds a viewer notices as a stall, and well
// inside what a client typically buffers before giving up on a join.
const tableRepeatTicks = 45000 // 500ms * 90000 ticks/sec / 1000ms

// pcrIntervalTicks is the target gap between PCRs on one PID: 50ms, well
// under the 100ms ISO 13818-1's T-STD model allows. A hardware demuxer built
// to that model, unlike ffmpeg, enforces the 100ms limit and drops the
// stream if it is missed. This muxer only gets a chance to emit anything
// when a frame arrives, so the actual gap between PCRs is this target plus
// however long the next frame takes to arrive; picking a target well under
// the limit leaves room for that before the hard limit is at risk, which
// tying the PCR to keyframes alone did not, since a keyframe interval is
// itself close to a full second at a typical GOP length.
const pcrIntervalTicks = 4500 // 50ms * 90000 ticks/sec / 1000ms

// pcrDecodeDelayTicks holds the PCR a fixed amount behind the PTS it
// accompanies. A PCR equal to its PTS asserts a decoder buffer with zero
// fill time, which a lenient client like ffmpeg does not check but a T-STD
// strict one can reject outright. 100ms is a conservative, arbitrary but
// fixed buffering allowance; these cameras never emit B frames, so DTS and
// PTS are the same value and there is no per-frame reordering delay to
// account for on top of it.
const pcrDecodeDelayTicks = 9000

// ptsPCRMask keeps only the low 33 bits of a PTS or PCR base before it is
// written to the wire. Both fields are 33 bits and wrap at the same modulus,
// so a PTS or PCR past 2^33 ticks (about 26.5 hours) already came out
// correct by truncation alone, since writePTS and writeAdaptation only ever
// read the bit ranges that survive it. Masking here makes that wrap a stated
// fact rather than a side effect nobody meant to rely on.
const ptsPCRMask = 1<<33 - 1

// Muxer turns decoded camera frames into an MPEG-TS byte stream. It holds no
// socket and does no I/O: callers own delivery, this just produces bytes.
type Muxer struct {
	streamType    byte
	audioType     byte // 0 means this muxer carries no audio
	clock         *Clock
	cc            continuity
	sawKeyframe   bool
	sentTables    bool
	lastTablesPTS uint64
	sentPCR       bool
	lastPCRPTS    uint64
	droppedAudio  int
}

// continuity tracks the 4 bit continuity counter per PID. Each PID counts
// independently; sharing one counter across PIDs is a common mistake that
// makes a decoder see false discontinuities.
type continuity map[PID]byte

func (c continuity) next(pid PID) byte {
	v := c[pid]
	c[pid] = (v + 1) & 0x0F
	return v
}

// last returns the counter value most recently handed out by next, for a
// packet that must repeat it rather than advance it. Calling this before
// next has ever been called for the PID is meaningless, but harmless: there
// is no earlier packet on that PID for a decoder to check continuity
// against yet.
func (c continuity) last(pid PID) byte {
	return (c[pid] - 1) & 0x0F
}

// NewMuxer builds a muxer for the given codec, "h264" or "h265", matched case
// insensitively. baichuan.Frame.Codec comes off the wire as "H264"/"H265",
// while docs, tests and a future TOML config all write it lowercase; folding
// the case here, at the one place every caller must pass through, means a
// caller does not need to remember to normalize it and a config value like
// "H265" fails as an unsupported-codec message would read like a camera
// fault instead of a config typo.
func NewMuxer(codec string) (*Muxer, error) {
	var st byte
	switch strings.ToLower(codec) {
	case "h264":
		st = StreamTypeH264
	case "h265":
		st = StreamTypeHEVC
	default:
		return nil, fmt.Errorf("ts: unsupported codec %q", codec)
	}
	return &Muxer{
		streamType: st,
		clock:      NewClock(),
		cc:         continuity{},
	}, nil
}

// NewMuxerWithAudio builds a muxer that also carries an audio track on its
// own PID. audioCodec is matched case insensitively for the same reason
// videoCodec is in NewMuxer.
//
// Only "aac" is accepted. These cameras also emit ADPCM, which has no
// registered MPEG-TS stream type; the previous tool silently dropped audio
// entirely regardless of the recorder's configured role, and nothing ever
// alerted on it. Relabelling ADPCM as AAC would decode as noise, and
// transcoding it would put ffmpeg back in the serving path, which this
// project exists to avoid. So an ADPCM frame is counted (DroppedAudio) and
// dropped, never silently accepted.
func NewMuxerWithAudio(videoCodec, audioCodec string) (*Muxer, error) {
	m, err := NewMuxer(videoCodec)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(audioCodec) {
	case "aac":
		m.audioType = StreamTypeAAC
	default:
		return nil, fmt.Errorf("ts: unsupported audio codec %q", audioCodec)
	}
	return m, nil
}

// DroppedAudio reports how many audio frames were dropped for lack of a
// supported codec, such as ADPCM. It should stay zero on a healthy stream
// configured with an audio role; a climbing count means the camera is
// sending a codec this muxer cannot carry.
func (m *Muxer) DroppedAudio() int { return m.droppedAudio }

// Header returns a freshly built PAT and PMT pair, for a client that joins
// after the stream has already started. internal/hub calls this once, when
// the codec first becomes known, and caches the result for every future
// joiner; see hub.SetHeader for why that one frozen snapshot is acceptable
// rather than a fresh call per joiner.
func (m *Muxer) Header() []byte {
	out := make([]byte, 0, 2*PacketSize)
	out = append(out, writePAT(nil, m.cc.next(PIDPAT))...)
	out = append(out, writePMT(nil, m.cc.next(PIDPMT), m.streamType, m.audioType)...)
	return out
}

// Frame encodes one decoded camera frame, returning the transport packets to
// send. Video frames go on PIDVideo; an audio frame goes on PIDAudio if this
// muxer was built with NewMuxerWithAudio and the codec matches what it was
// built for, otherwise it is dropped (silently for a muxer that never
// declared audio at all, counted via DroppedAudio when audio was declared
// but the frame's codec, such as ADPCM, is not carried). Info frames and any
// frame arriving before the first video keyframe are dropped too.
//
// It wraps FrameWithKey, discarding the keyframe flag, for every caller that
// only ever wanted the bytes.
func (m *Muxer) Frame(f baichuan.Frame) []byte {
	chunk, _ := m.FrameWithKey(f)
	return chunk
}

// FrameWithKey is Frame plus a flag telling the caller whether chunk begins
// a video keyframe, meaning it contains that frame's PES start (and, when
// due, the PAT/PMT ahead of it). Only the muxer knows this: a subscriber
// that joins mid GOP has to wait for exactly this boundary before its
// decoder sees anything, since a decoder handed slices before the parameter
// sets that describe them logs reference errors until the next I frame
// (measured against a live camera: about 9 error groups in the first 60
// seconds of a fresh join, then clean). startsKeyframe is only ever true for
// a video I frame; audio and P frames always report false.
func (m *Muxer) FrameWithKey(f baichuan.Frame) (chunk []byte, startsKeyframe bool) {
	if f.Kind == baichuan.FrameAAC || f.Kind == baichuan.FrameADPCM {
		return m.audioFrame(f), false
	}
	if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
		return nil, false
	}
	isKey := f.Kind == baichuan.FrameIFrame
	if !m.sawKeyframe {
		if !isKey {
			// Starting a decoder mid GOP only makes it complain about
			// references it never saw, so nothing goes out until an I frame.
			return nil, false
		}
		m.sawKeyframe = true
	}

	pts := m.clock.PTS(f.Micros)

	var out []byte
	if !m.sentTables || pts-m.lastTablesPTS >= tableRepeatTicks {
		out = append(out, m.Header()...)
		m.sentTables = true
		m.lastTablesPTS = pts
	}

	// HEVC frames from these cameras carry a proprietary prefix before the
	// first NAL start code; Video() strips it. Feeding Data directly produces
	// sporadic decoder errors like "cu_qp_delta out of range".
	payload := f.Video()
	pes := buildPES(videoStreamID, pts, payload)

	if !m.sentPCR || pts-m.lastPCRPTS >= pcrIntervalTicks {
		// The PCR goes out on its own adaptation-field-only packet ahead of
		// the PES. Putting it in the PES packet's own adaptation field would
		// work too, but it is not worth the bookkeeping: this way every
		// PES-start packet is a plain payload-only packet with the PES
		// header sitting straight at byte 4, which is what every consumer of
		// this stream, including this package's own tests, expects to find.
		pcr := pts
		if pcr > pcrDecodeDelayTicks {
			pcr -= pcrDecodeDelayTicks
		} else {
			pcr = 0
		}
		out = append(out, m.pcrPacket(pcr)...)
		m.sentPCR = true
		m.lastPCRPTS = pts
	}
	out = append(out, m.packetisePES(PIDVideo, pes)...)
	return out, isKey
}

// audioFrame encodes one audio frame onto PIDAudio, or drops it.
//
// Unlike video, an audio frame has no GOP structure to wait on: any AAC
// frame is self contained and a decoder can start on any of them. It does
// still wait for this muxer's own tables (PAT/PMT) to have gone out at
// least once, though, so nothing carrying stream_id 0xC0 can reach a client
// that has no PMT yet to say what that stream_id means. internal/stream
// never triggers this in practice: it always builds a muxer from a video
// frame, and that same call sends the tables before returning, so the two
// events are never actually reordered there. This exists for a direct
// ts.Muxer caller that feeds audio before ever feeding video, which would
// otherwise see audio PES packets with no header at all.
func (m *Muxer) audioFrame(f baichuan.Frame) []byte {
	if m.audioType == 0 {
		// This muxer was never given an audio codec: audio is dropped
		// without counting it, the same as NewMuxer's video-only callers
		// have always seen for any non video frame.
		return nil
	}
	if !m.sentTables {
		return nil
	}
	if f.Kind != baichuan.FrameAAC {
		// ADPCM, or anything else that is not the codec this muxer was
		// built for. Counted, never relabelled: see NewMuxerWithAudio.
		m.droppedAudio++
		return nil
	}
	pts := m.clock.PTS(f.Micros)
	pes := buildPES(audioStreamID, pts, f.Data)
	return m.packetisePES(PIDAudio, pes)
}

// pcrPacket builds a transport packet carrying nothing but a PCR, so a
// player has a clock reference before the frame's payload even starts.
// Without a PCR early in the stream many players refuse to start at all.
//
// It repeats the last counter value used on PIDVideo rather than advancing
// it. ISO 13818-1 requires the counter to hold steady on a packet with no
// payload (adaptation_field_control '10'): the value must match the packet
// before it, not pre-empt the payload packet after it, or a strict demuxer
// sees a false gap and reports a corrupt packet on every single keyframe.
func (m *Muxer) pcrPacket(pcr uint64) []byte {
	pkt := make([]byte, PacketSize)
	writeHeader(pkt, PIDVideo, m.cc.last(PIDVideo), false)
	pkt[3] = pkt[3]&0x0F | 0x20 // adaptation field only, no payload
	writeAdaptation(pkt[4:], PacketSize-4, true, pcr)
	return pkt
}

// videoStreamID and audioStreamID are the PES stream_id values for this
// muxer's two elementary streams. 0xE0 is the standard "first video stream"
// id; 0xC0 is the matching "first audio stream" id. A demuxer uses this,
// not the PID, to sort PES packets by track once it has parsed the PMT.
const (
	videoStreamID = 0xE0
	audioStreamID = 0xC0
)

// buildPES wraps an elementary stream payload in a PES header.
//
// The packet length field is left 0, which the format explicitly allows for
// video, and is the only sane choice here since a frame can exceed the 16 bit
// field's 65535 byte limit. Audio frames are always small enough to fit the
// 16 bit field, but leaving it 0 for both keeps this one code path rather
// than branching a length calculation in for audio alone.
//
// Only a PTS is written, no DTS, and that is not an omission: these cameras
// never emit B frames, so decode order and presentation order are the same
// and DTS would always equal PTS. A PES header can carry PTS alone (flag
// '10' rather than '11'), which is what byte 7 below sets.
func buildPES(streamID byte, pts uint64, payload []byte) []byte {
	pes := make([]byte, 0, 19+len(payload))
	pes = append(pes, 0x00, 0x00, 0x01, streamID) // start code, stream id
	pes = append(pes, 0x00, 0x00)                 // PES packet length: unbounded
	// 0x84: marker bits '10', then data_alignment_indicator set, since every
	// PES here starts exactly on an access unit boundary; original_or_copy is
	// left 0, which per the spec means "copy" rather than "original" (this is
	// a live re-encode of the camera's own stream, not virgin source), not
	// the "original" the field name misleadingly suggests at a glance.
	pes = append(pes, 0x84, 0x80) // flags, PTS present (no DTS)
	pes = append(pes, 0x05)       // PES header data length: PTS only
	ptsField := make([]byte, 5)
	writePTS(ptsField, pts)
	pes = append(pes, ptsField...)
	pes = append(pes, payload...)
	return pes
}

// writePTS encodes a 33 bit timestamp into the 5 byte field the PES header
// uses. The value is split across the bytes with marker bits set between the
// pieces, which is why this is not a plain big endian write.
func writePTS(dst []byte, pts uint64) {
	pts &= ptsPCRMask
	dst[0] = 0x21 | byte(pts>>29)&0x0E
	dst[1] = byte(pts >> 22)
	dst[2] = 0x01 | byte(pts>>14)&0xFE
	dst[3] = byte(pts >> 7)
	dst[4] = 0x01 | byte(pts<<1)&0xFE
}

// packetisePES splits a PES packet across 188 byte transport packets on pid,
// each with the PES (or its continuation) as the entire payload. pid is
// either PIDVideo or PIDAudio; each carries its own continuity counter (see
// the continuity type), so interleaving the two tracks never disturbs
// either one's count.
//
// The last packet is usually short. It used to be padded with plain zero
// bytes rather than a stuffed adaptation field, on the reasoning that NAL
// start codes are prefixed with leading_zero_8bits and RBSP data may be
// followed by trailing_zero_8bits, so an Annex B parser skips zero padding
// between NALs as a matter of course. That is true for a parser reading the
// byte stream directly, but Frigate remuxes this to MP4, and its annexb to
// HVCC converter folds trailing zero bytes into the preceding NAL's length
// field instead of discarding them, so every recording ended up with junk
// appended to its last NAL. A proper adaptation field costs a handful of
// bytes and avoids that. Audio frames are ADTS, not Annex B, so this
// padding scheme buys them nothing, but sharing the one code path is worth
// more than a padding style optimised for a format audio does not use.
func (m *Muxer) packetisePES(pid PID, pes []byte) []byte {
	var out []byte
	first := true
	for len(pes) > 0 {
		n := len(pes)
		if n > PacketSize-4 {
			n = PacketSize - 4
		}
		afTotal := 0
		if len(pes) <= PacketSize-4 {
			// Last packet of the PES: stretch the adaptation field with
			// stuffing so the payload ends exactly at the packet boundary.
			afTotal = PacketSize - 4 - n
		}

		pkt := make([]byte, PacketSize)
		writeHeader(pkt, pid, m.cc.next(pid), first)
		if afTotal > 0 {
			pkt[3] = pkt[3]&0x0F | 0x30 // adaptation field and payload both present
			writeAdaptation(pkt[4:], afTotal, false, 0)
		}
		copy(pkt[4+afTotal:], pes[:n])
		pes = pes[n:]
		first = false
		out = append(out, pkt...)
	}
	return out
}

// writeAdaptation fills an adaptation field of afTotal bytes (including its
// own length byte) into dst, optionally carrying a PCR, padding the rest with
// stuffing bytes.
func writeAdaptation(dst []byte, afTotal int, withPCR bool, pcr uint64) {
	dst[0] = byte(afTotal - 1) // adaptation_field_length excludes itself
	if afTotal == 1 {
		// Length 0 is a valid one byte adaptation field: pure stuffing, no
		// flags byte at all.
		return
	}
	i := 2 // dst[1] is the flags byte, filled in below
	if withPCR {
		pcr &= ptsPCRMask
		dst[1] = 0x10 // PCR_flag set, everything else clear
		// PCR is a 33 bit 90kHz base plus a 9 bit extension, packed into 6
		// bytes. The extension is left 0: this PCR is derived from the same
		// 90kHz PTS clock, which has no finer resolution to offer, so a
		// nonzero extension would only be misleading precision.
		dst[i+0] = byte(pcr >> 25)
		dst[i+1] = byte(pcr >> 17)
		dst[i+2] = byte(pcr >> 9)
		dst[i+3] = byte(pcr >> 1)
		dst[i+4] = byte(pcr<<7) | 0x7E
		dst[i+5] = 0x00
		i += 6
	} else {
		dst[1] = 0x00
	}
	for ; i < afTotal; i++ {
		dst[i] = 0xFF
	}
}
