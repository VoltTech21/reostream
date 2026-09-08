package ts

import (
	"fmt"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// tableRepeatInterval is how often PAT and PMT are resent among the video
// packets, in frames. A client that joins mid stream cannot decode anything
// until it has both tables, so they cannot only be sent once at the start.
const tableRepeatInterval = 100

// Muxer turns decoded camera frames into an MPEG-TS byte stream. It holds no
// socket and does no I/O: callers own delivery, this just produces bytes.
type Muxer struct {
	streamType        byte
	clock             *Clock
	cc                continuity
	sawKeyframe       bool
	framesSinceTables int
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

// NewMuxer builds a muxer for the given codec, "h264" or "h265".
func NewMuxer(codec string) (*Muxer, error) {
	var st byte
	switch codec {
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

// Header returns a freshly built PAT and PMT pair, for a client that joins
// after the stream has already started.
func (m *Muxer) Header() []byte {
	out := make([]byte, 0, 2*PacketSize)
	out = append(out, writePAT(nil, m.cc.next(PIDPAT))...)
	out = append(out, writePMT(nil, m.cc.next(PIDPMT), m.streamType)...)
	return out
}

// Frame encodes one decoded camera frame, returning the transport packets to
// send. It returns nil for audio and info frames, which this muxer does not
// carry, and for any frame arriving before the first keyframe.
func (m *Muxer) Frame(f baichuan.Frame) []byte {
	if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
		return nil
	}
	if !m.sawKeyframe {
		if f.Kind != baichuan.FrameIFrame {
			// Starting a decoder mid GOP only makes it complain about
			// references it never saw, so nothing goes out until an I frame.
			return nil
		}
		m.sawKeyframe = true
	}

	var out []byte
	if m.framesSinceTables == 0 {
		out = append(out, m.Header()...)
	}
	m.framesSinceTables++
	if m.framesSinceTables >= tableRepeatInterval {
		m.framesSinceTables = 0
	}

	pts := m.clock.PTS(f.Micros)
	// HEVC frames from these cameras carry a proprietary prefix before the
	// first NAL start code; Video() strips it. Feeding Data directly produces
	// sporadic decoder errors like "cu_qp_delta out of range".
	payload := f.Video()
	pes := buildPES(pts, payload)

	if f.Kind == baichuan.FrameIFrame {
		// The PCR goes out on its own adaptation-field-only packet ahead of
		// the PES. Putting it in the PES packet's own adaptation field would
		// work too, but it is not worth the bookkeeping: this way every
		// PES-start packet is a plain payload-only packet with the PES
		// header sitting straight at byte 4, which is what every consumer of
		// this stream, including this package's own tests, expects to find.
		out = append(out, m.pcrPacket(pts)...)
	}
	out = append(out, m.packetisePES(pes)...)
	return out
}

// pcrPacket builds a transport packet carrying nothing but a PCR, so a
// player has a clock reference before the frame's payload even starts.
// Without a PCR early in the stream many players refuse to start at all.
func (m *Muxer) pcrPacket(pts uint64) []byte {
	pkt := make([]byte, PacketSize)
	writeHeader(pkt, PIDVideo, m.cc.next(PIDVideo), false)
	pkt[3] = pkt[3]&0x0F | 0x20 // adaptation field only, no payload
	writeAdaptation(pkt[4:], PacketSize-4, true, pts)
	return pkt
}

// buildPES wraps an elementary stream payload in a PES header.
//
// The packet length field is left 0, which the format explicitly allows for
// video, and is the only sane choice here since a frame can exceed the 16 bit
// field's 65535 byte limit.
func buildPES(pts uint64, payload []byte) []byte {
	pes := make([]byte, 0, 19+len(payload))
	pes = append(pes, 0x00, 0x00, 0x01, 0xE0) // start code, stream id
	pes = append(pes, 0x00, 0x00)             // PES packet length: unbounded
	pes = append(pes, 0x80, 0x80)             // flags: original, PTS present
	pes = append(pes, 0x05)                   // PES header data length: PTS only
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
	dst[0] = 0x21 | byte(pts>>29)&0x0E
	dst[1] = byte(pts >> 22)
	dst[2] = 0x01 | byte(pts>>14)&0xFE
	dst[3] = byte(pts >> 7)
	dst[4] = 0x01 | byte(pts<<1)&0xFE
}

// packetisePES splits a PES packet across 188 byte transport packets on
// PIDVideo, each with the PES (or its continuation) as the entire payload.
//
// The last packet is usually short. Rather than mark the remainder with a
// stuffed adaptation field, it is left as the zero bytes Go already
// initialises the packet to: NAL start codes are prefixed with
// leading_zero_8bits and RBSP data may be followed by trailing_zero_8bits, so
// an Annex B parser skips zero padding between NALs as a matter of course. A
// stuffed adaptation field would be more strictly to the letter of the
// transport stream spec, but the extra bookkeeping buys nothing a real
// decoder needs.
func (m *Muxer) packetisePES(pes []byte) []byte {
	var out []byte
	first := true
	for len(pes) > 0 {
		pkt := make([]byte, PacketSize)
		writeHeader(pkt, PIDVideo, m.cc.next(PIDVideo), first)
		n := len(pes)
		if n > PacketSize-4 {
			n = PacketSize - 4
		}
		copy(pkt[4:], pes[:n])
		pes = pes[n:]
		first = false
		out = append(out, pkt...)
	}
	return out
}

// writeAdaptation fills an adaptation field of afTotal bytes (including its
// own length byte) into dst, optionally carrying a PCR, padding the rest with
// stuffing bytes.
func writeAdaptation(dst []byte, afTotal int, withPCR bool, pts uint64) {
	dst[0] = byte(afTotal - 1) // adaptation_field_length excludes itself
	if afTotal == 1 {
		// Length 0 is a valid one byte adaptation field: pure stuffing, no
		// flags byte at all.
		return
	}
	i := 2 // dst[1] is the flags byte, filled in below
	if withPCR {
		dst[1] = 0x10 // PCR_flag set, everything else clear
		// PCR is a 33 bit 90kHz base plus a 9 bit extension, packed into 6
		// bytes. The extension is left 0: the PTS clock this is derived from
		// has no finer resolution to offer, so a nonzero extension would only
		// be misleading precision.
		pcr := pts
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
