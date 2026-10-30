package baichuan

import "encoding/binary"

// FrameKind identifies a media packet type.
type FrameKind int

const (
	FrameUnknown FrameKind = iota
	FrameInfo
	FrameIFrame
	FramePFrame
	FrameAAC
	FrameADPCM
)

// Video returns the frame's video data starting at its first NAL start code.
//
// HEVC frames from these cameras carry a variable-length proprietary prefix
// before the first start code -- 80, 112 or 152 bytes have been observed --
// while H.264 frames start at the start code directly. Feeding that prefix to
// a decoder produces sporadic errors, so anything writing an elementary
// stream should use this rather than Data.
func (f Frame) Video() []byte {
	if i := indexStartCode(f.Data); i > 0 {
		return f.Data[i:]
	}
	return f.Data
}

func indexStartCode(b []byte) int {
	for i := 0; i+4 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i
		}
	}
	return -1
}

// hevcNALHeaderValid reports whether the two bytes after a start code are a
// well-formed HEVC NAL header for these single-layer cameras: a defined NAL
// type, nuh_layer_id 0, and nuh_temporal_id_plus1 >= 1. The proprietary
// prefix metadata occasionally contains a stray 00 00 00 01, and the bytes
// after it fail this check (bad layer id or temporal id), which is how a
// false start code in the metadata is told apart from the real first NAL.
func hevcNALHeaderValid(b0, b1 byte) bool {
	t := (b0 >> 1) & 0x3f
	validType := t <= 9 || (t >= 16 && t <= 21) || (t >= 32 && t <= 40)
	layerID := (uint16(b0&0x01) << 5) | uint16(b1>>3)
	tidPlus1 := b1 & 0x07
	return validType && layerID == 0 && tidPlus1 >= 1
}

// hevcFirstNAL returns the offset of the first start code that begins a
// structurally valid HEVC NAL, skipping false start codes embedded in the
// camera prefix metadata. Returns -1 if none is found.
func hevcFirstNAL(b []byte) int {
	for i := 0; i+6 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 && hevcNALHeaderValid(b[i+4], b[i+5]) {
			return i
		}
	}
	return -1
}

// h264Profiles are the profile_idc values defined by the H.264 spec. A byte
// that is not one of these cannot begin a real SPS, which is the check that
// tells a true SPS from prefix metadata that happens to look like one.
var h264Profiles = map[byte]bool{
	44: true, 66: true, 77: true, 83: true, 86: true, 88: true, 100: true,
	110: true, 118: true, 122: true, 128: true, 134: true, 135: true,
	138: true, 139: true, 244: true,
}

// h264NALHeaderValid reports whether b begins a well-formed H.264 NAL for
// these cameras, where b starts at the NAL header byte.
//
// The first version of this checked only forbidden_zero_bit and
// nal_unit_type 1-23, which accepts 23 of the 128 bytes a forbidden-zero
// byte can hold. Prefix metadata clears that bar about one frame in two
// thousand: a measured capture of the fisheye carried
//
//	42 98 a7 fb 09 00 01 01 00 01 02 02 00 f5 04 00 ...
//
// where 0x42 is nal_unit_type 2, and another carried 0x27, an SPS whose
// profile_idc was 123. Each one ended the prefix search early, so the
// metadata was published as if it were the start of the picture. ffmpeg
// then reported "non-existing PPS 1 referenced" or rejected the SPS, and
// Frigate discarded the recording segment that began there as having no
// video stream. Roughly one segment every two minutes, on the only H.264
// camera in the fleet, for months.
//
// So this now spends every constraint the header offers, the way the HEVC
// check does:
//
//   - only the six NAL types these cameras emit
//   - nal_ref_idc non-zero on SPS, PPS and IDR, which the spec requires,
//     and zero on SEI and AUD, which it also requires
//   - for an SPS, a profile_idc the spec defines
func h264NALHeaderValid(b []byte) bool {
	if len(b) == 0 || b[0]&0x80 != 0 {
		return false
	}
	ref := (b[0] >> 5) & 0x03
	switch b[0] & 0x1f {
	case 1: // non-IDR slice: may be disposable, so any nal_ref_idc
		return true
	case 5, 8: // IDR slice, PPS
		return ref != 0
	case 7: // SPS
		return ref != 0 && len(b) >= 2 && h264Profiles[b[1]]
	case 6, 9: // SEI, access unit delimiter
		return ref == 0
	}
	return false
}

// h264StartsAccessUnit reports whether b begins a NAL that can legitimately
// be the first one of a keyframe: parameter sets, or the delimiter and SEI
// that may precede them. An IDR slice is deliberately not here, because on
// these cameras the picture always opens with its parameter sets.
func h264StartsAccessUnit(b []byte) bool {
	if !h264NALHeaderValid(b) {
		return false
	}
	switch b[0] & 0x1f {
	case 6, 7, 9:
		return true
	}
	return false
}

// h264FirstNAL returns the offset of the first start code that begins a
// valid H.264 NAL, skipping false start codes in the prefix metadata.
//
// keyframe narrows what counts. Tightening the header check cut this
// corruption by about 94% on the live fleet, from 30 discarded recording
// segments an hour to 2, but it could not go further on its own: an
// ordinary P-frame slice is nal_unit_type 1 with any nal_ref_idc, four byte
// values that metadata hits by chance, and rejecting those would reject
// real frames.
//
// What makes the rest reachable is that the damage is not uniform. A
// recording segment begins at a keyframe, so a segment is only ruined when
// the corruption lands on an I-frame; the same bytes inside a P-frame cost
// one picture and nothing else. An I-frame's first NAL is never a plain
// slice on these cameras, so for those the search can insist on a parameter
// set and shut the door P-frames have to leave open.
//
// The strict pass falls back to the general one rather than failing: a
// camera that opens a keyframe some other way then behaves exactly as it
// did before, instead of having its metadata treated as picture.
func h264FirstNAL(b []byte, keyframe bool) int {
	if keyframe {
		if i := h264SearchNAL(b, h264StartsAccessUnit); i >= 0 {
			return i
		}
	}
	return h264SearchNAL(b, h264NALHeaderValid)
}

// h264SearchNAL returns the offset of the first start code whose NAL header
// satisfies ok.
func h264SearchNAL(b []byte, ok func([]byte) bool) int {
	for i := 0; i+5 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 && ok(b[i+4:]) {
			return i
		}
	}
	return -1
}

func (k FrameKind) String() string {
	switch k {
	case FrameInfo:
		return "info"
	case FrameIFrame:
		return "iframe"
	case FramePFrame:
		return "pframe"
	case FrameAAC:
		return "aac"
	case FrameADPCM:
		return "adpcm"
	}
	return "unknown"
}

// Frame is one decoded media packet.
//
// Micros is the camera's own capture time. Emitting it downstream is what
// keeps timing correct: a raw elementary stream carries no timing at all, so
// anything muxing it has to invent timestamps, and invents them badly.
type Frame struct {
	Kind   FrameKind
	Codec  string
	Micros uint32
	Data   []byte
	Width  int
	Height int
	FPS    int
}

var (
	magicInfoV1 = []byte{0x31, 0x30, 0x30, 0x31}
	magicInfoV2 = []byte{0x31, 0x30, 0x30, 0x32}
	magicIFrame = []byte{0x30, 0x30, 0x64, 0x63}
	magicPFrame = []byte{0x30, 0x31, 0x64, 0x63}
	magicAAC    = []byte{0x30, 0x35, 0x77, 0x62}
	magicADPCM  = []byte{0x30, 0x31, 0x77, 0x62}
)

// Depacketizer reassembles media packets from the byte stream carried in
// message id 3. A media packet may straddle message boundaries, so bytes are
// buffered until a whole packet is present.
type Depacketizer struct {
	buf     []byte
	filler  int
	skipped int
	lost    int
	gap     bool
}

func NewDepacketizer() *Depacketizer { return &Depacketizer{} }

// Write adds the payload of one message. startsPacket must be true when the
// message's extension header declared binaryData, which is what marks the
// start of a new media packet: anything still buffered at that point is
// trailing filler from the previous packet's last message and is dropped.
// The presence of an extension header alone is not the boundary, since some
// cameras put one on continuation messages too (see Message.StartsPacket).
func (d *Depacketizer) Write(b []byte, startsPacket bool) {
	if startsPacket {
		// A video packet still waiting for its bytes is not filler: it
		// was cut short, and every frame after it refers to a picture
		// that never arrived. Note it so the stream can skip to the next
		// keyframe rather than hand a decoder broken references (#17).
		if isVideoMagic(d.buf) {
			d.lost++
			d.gap = true
		} else {
			d.filler += len(d.buf)
		}
		d.buf = d.buf[:0]
	}
	d.buf = append(d.buf, b...)
}

// Lost reports video packets cut short by the start of the next one.
func (d *Depacketizer) Lost() int { return d.lost }

// TakeGap reports whether a video packet was lost since the last call.
func (d *Depacketizer) TakeGap() bool {
	g := d.gap
	d.gap = false
	return g
}

// Skipped reports bytes discarded because the buffer did not begin with a
// packet magic where one was required. It should stay zero; a climbing count
// means packets are being mis-framed.
func (d *Depacketizer) Skipped() int { return d.skipped }

// Filler reports bytes dropped between a packet's end and the end of the
// message carrying it. A healthy stream has a nonzero, slowly growing count:
// this is the camera's own padding, not a parsing error.
func (d *Depacketizer) Filler() int { return d.filler }

// maxFramePrefix bounds the search for the first NAL start code in a media
// packet. The camera metadata that precedes it has been measured at 80, 104,
// 112, 144, 152, 176, 184, 328 and 352 bytes across the fleet, and it varies
// frame to frame on the same stream, so this is not a length to guess at:
// a bound of 256 looked generous against the fisheye and then cut every
// frame whose prefix ran past it on the pano's substream, which is the same
// dropped-tail corruption this search exists to prevent.
//
// The cap is here only so a packet with a corrupt header cannot scan an
// entire picture looking for a byte pattern that will eventually appear by
// chance. 4096 is an order of magnitude above anything observed and still
// a small fraction of any real frame.
const maxFramePrefix = 4096

// prefixVerdict says how much the packet header could be trusted about where
// its picture begins.
type prefixVerdict int

const (
	prefixStated   prefixVerdict = iota // the header said, and it checks out
	prefixNeedMore                      // too little buffered to tell yet
	prefixUnusable                      // the header did not say usefully
)

// statedPrefix returns the metadata length declared by the packet header.
//
// The word at offset 12 holds it, measured from the end of the 24 bytes both
// packet kinds share rather than from the end of this packet's header, so an
// I frame's extra time_t and unknown word are counted inside it. That is why
// an I frame carrying 112 bytes of metadata declares 120.
//
// This is the whole corruption problem solved by reading instead of
// guessing. searchPrefix has to recognize the first NAL by its header bits,
// and a P frame's first NAL is type 1 with any nal_ref_idc -- four byte
// values that metadata hits by chance about one frame in two thousand.
// Every such miss both leaks metadata into the picture and cuts the same
// number of bytes off its tail. The declared length has no such failure
// mode: across the committed captures it lands on a NAL start code in 287
// of 287 complete packets, H.264 and HEVC, I frames and P frames alike.
//
// It is still checked rather than trusted. The bytes it points at must
// begin a start code and a NAL header the codec allows, because a camera
// that puts something else in that word must not be able to make this worse
// than the search it replaces; it falls back to the search when the claim
// does not hold up, which is also what happens for a camera whose metadata
// this field does not describe.
// settled says the packet is already complete as the search reads it, so no
// further bytes are coming and a declaration that cannot be checked by now
// never will be.
func statedPrefix(buf []byte, hdr int, codec string, settled bool) (int, prefixVerdict) {
	const base = 24 // the header both packet kinds share
	if len(buf) < base+4 {
		return 0, prefixNeedMore
	}
	stated := int(binary.LittleEndian.Uint32(buf[12:]))
	prefix := base + stated - hdr
	if prefix < 0 || prefix > maxFramePrefix {
		return 0, prefixUnusable
	}
	// A NAL header is 2 bytes for HEVC and 1 for H.264; take 2 either way
	// so one length covers both and the check below cannot read past it.
	if len(buf) < hdr+prefix+6 {
		if settled {
			return 0, prefixUnusable
		}
		return 0, prefixNeedMore
	}
	at := buf[hdr+prefix:]
	if !(at[0] == 0 && at[1] == 0 && at[2] == 0 && at[3] == 1) {
		return 0, prefixUnusable
	}
	switch codec {
	case "H265", "h265":
		if !hevcNALHeaderValid(at[4], at[5]) {
			return 0, prefixUnusable
		}
	case "H264", "h264":
		if !h264NALHeaderValid(at[4:]) {
			return 0, prefixUnusable
		}
	}
	return prefix, prefixStated
}

// searchPrefix finds the picture by looking for its first NAL, for packets
// whose header does not declare where it starts.
//
// HEVC needs a NAL-header-aware search: these cameras put a proprietary
// prefix before the picture that can contain a false 00 00 00 01, and
// stopping there leaks a garbage reserved-type NAL into the frame and
// truncates its tail.
func searchPrefix(buf []byte, hdr int, codec string, kind FrameKind) int {
	region := buf[hdr:min(hdr+maxFramePrefix, len(buf))]
	var prefix int
	switch codec {
	case "H265", "h265":
		prefix = hevcFirstNAL(region)
	case "H264", "h264":
		prefix = h264FirstNAL(region, kind == FrameIFrame)
	default:
		prefix = indexStartCode(region)
	}
	if prefix < 0 {
		return 0
	}
	return prefix
}

// isVideoMagic reports a video packet: "00dc" for an I frame and "01dc"
// for a P frame, where the first digit is the channel. Channel 0 is every
// camera's only picture; the TrackMix zoom lens, channel 1, sends "10dc"
// and "11dc" (captured 2026-10-24).
func isVideoMagic(b []byte) bool {
	return len(b) >= 4 && b[0] >= '0' && b[0] <= '9' && (b[1] == '0' || b[1] == '1') && b[2] == 'd' && b[3] == 'c'
}

// isAudio reports an audio packet of the given kind on any channel: the
// first digit is the channel, as for video, so the TrackMix zoom lens sends
// its AAC as "15wb" (firmware v3.0.0.5428, emulated 2026-10-29).
func isAudio(b, magic []byte) bool {
	return len(b) >= 4 && b[0] >= '0' && b[0] <= '9' && b[1] == magic[1] &&
		b[2] == magic[2] && b[3] == magic[3]
}

func match(b, magic []byte) bool {
	return len(b) >= 4 && b[0] == magic[0] && b[1] == magic[1] &&
		b[2] == magic[2] && b[3] == magic[3]
}

// Next returns the next complete frame, or false if more bytes are needed.
func (d *Depacketizer) Next() (Frame, bool) {
	for {
		if len(d.buf) < 8 {
			return Frame{}, false
		}
		switch {
		case isVideoMagic(d.buf):
			// magic(4) type(4) size(4) unknown(4) micros(4) unknown(4),
			// and for I frames a further time_t(4) and unknown(4).
			hdr, kind := 24, FramePFrame
			if d.buf[1] == '0' {
				hdr, kind = 32, FrameIFrame
			}
			if len(d.buf) < hdr {
				return Frame{}, false
			}
			size := int(binary.LittleEndian.Uint32(d.buf[8:]))
			if size < 0 || size > 1<<24 {
				d.buf, d.skipped = d.buf[1:], d.skipped+1
				continue
			}
			if len(d.buf) < hdr+size {
				return Frame{}, false
			}
			// size counts the coded picture only, not the camera metadata
			// that sits between the packet header and the first NAL start
			// code, so a packet occupies hdr+prefix+size bytes. Cameras that
			// send no metadata have prefix 0 and are unaffected.
			//
			// Measured on the fisheye: prefix 104, 176 or 184 bytes varying
			// per frame, and consuming hdr+size instead cut that many bytes
			// off the end of every frame. The picture still decoded, since
			// only the last macroblock rows were missing, which is why this
			// showed up as "error while decoding MB x 141..159" rather than
			// as a stream that failed outright.
			codec := string(trimNul(d.buf[4:8]))
			// The header says how long the metadata is, so the picture's
			// first byte is a fact to read rather than a pattern to hunt
			// for. Prefer that; see statedPrefix.
			// The search runs either way, as the fallback and as the judge
			// of whether the packet is already all here: if it is, and the
			// declared length still cannot be checked, then the declaration
			// disagrees with the packet's own length and is not to be
			// believed. Without that test a camera declaring something
			// absurd would make this wait for bytes that are never coming.
			search := searchPrefix(d.buf, hdr, codec, kind)
			prefix, verdict := statedPrefix(d.buf, hdr, codec, len(d.buf) >= hdr+search+size)
			switch verdict {
			case prefixNeedMore:
				return Frame{}, false
			case prefixUnusable:
				prefix = search
			}
			if len(d.buf) < hdr+prefix+size {
				return Frame{}, false
			}
			f := Frame{
				Kind:   kind,
				Codec:  codec,
				Micros: binary.LittleEndian.Uint32(d.buf[16:]),
				Data:   append([]byte(nil), d.buf[hdr+prefix:hdr+prefix+size]...),
			}
			d.consume(hdr + prefix + size)
			return f, true

		case isAudio(d.buf, magicAAC), isAudio(d.buf, magicADPCM):
			// magic(4) size(2) size(2)
			size := int(binary.LittleEndian.Uint16(d.buf[4:]))
			if len(d.buf) < 8+size {
				return Frame{}, false
			}
			kind := FrameAAC
			if isAudio(d.buf, magicADPCM) {
				kind = FrameADPCM
			}
			f := Frame{Kind: kind, Data: append([]byte(nil), d.buf[8:8+size]...)}
			d.consume(8 + size)
			return f, true

		case match(d.buf, magicInfoV1), match(d.buf, magicInfoV2):
			// magic(4) headerlen(4) width(4) height(4) unknown(1) fps(1) ...
			n := int(binary.LittleEndian.Uint32(d.buf[4:]))
			if n < 8 || n > 1<<16 {
				d.buf, d.skipped = d.buf[1:], d.skipped+1
				continue
			}
			if len(d.buf) < n {
				return Frame{}, false
			}
			f := Frame{Kind: FrameInfo}
			if n >= 16 {
				f.Width = int(binary.LittleEndian.Uint32(d.buf[8:]))
				f.Height = int(binary.LittleEndian.Uint32(d.buf[12:]))
			}
			if n > 17 {
				f.FPS = int(d.buf[17])
			}
			d.consume(n)
			return f, true

		default:
			// The v3.0.0.136 firmware of the RLC-410-5MP and RLC-422 sends
			// several whole packets back to back in one message after the
			// first, each zero-padded to 8 bytes, with no extension header to
			// mark them as packet starts. Step over that padding when a packet
			// follows it, and only then: up to 7 zero bytes, then a magic.
			if n := padBeforePacket(d.buf); n > 0 {
				d.buf, d.filler = d.buf[n:], d.filler+n
				continue
			}
			// Not at a packet boundary. Packets always begin at the start of a
			// message carrying an extension header, so anything here is
			// trailing filler from the packet just emitted: leave it for the
			// next packet start to discard rather than byte-scanning through
			// it, which risks matching a magic inside the filler.
			return Frame{}, false
		}
	}
}

// padBeforePacket returns how many zero bytes (1 to 7) precede a packet
// magic at the start of b, or 0 when b does not start that way.
func padBeforePacket(b []byte) int {
	for n := 1; n < 8 && n < len(b); n++ {
		if b[n-1] != 0 {
			return 0
		}
		at := b[n:]
		if isVideoMagic(at) || isAudio(at, magicAAC) || isAudio(at, magicADPCM) ||
			match(at, magicInfoV1) || match(at, magicInfoV2) {
			return n
		}
	}
	return 0
}

// consume drops a packet of length n. Any bytes left in the buffer afterwards
// are filler to the end of the message and are dropped when the next packet
// starts.
func (d *Depacketizer) consume(n int) { d.buf = d.buf[n:] }

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
