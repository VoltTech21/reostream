package baichuan

import (
	"encoding/binary"
	"errors"
)

// Message IDs.
const (
	MsgIDLogin = 1
	MsgIDVideo = 3
	MsgIDPing  = 93
	// MsgIDHeartBeat is wrong and is kept only so the reocam heartbeat
	// command still names something. Two firmware dispatch tables agree
	// that 5 is "replay start"; 5 came from rpc_msg_name(5) in the NVR's
	// internal IPC enum, where index 5 is MSG_APP_HB, a different
	// namespace entirely. That is why every model answers 421: the camera
	// is refusing a playback request, not declining a heartbeat. The
	// Baichuan dispatch table calls id 0 "heartbeat".
	MsgIDHeartBeat   = 5
	MsgIDSnap        = 109
	MsgIDTalkAbility = 10
	MsgIDTalkConfig  = 201
	MsgIDTalk        = 202
	MsgIDAbilityInfo = 151
)

// Message classes. The class determines the header length.
const (
	ClassLegacy   uint16 = 0x6514 // 20 bytes
	ClassModern20 uint16 = 0x6614 // 20 bytes
	ClassModern24 uint16 = 0x6414 // 24 bytes
	ClassZero     uint16 = 0x0000 // 24 bytes
)

// Negotiation bytes observed on the wire. The protocol notes describe values
// 0x01..0x03 here, but this firmware sends 0x12 and replies 0x12/0xdd, so we
// send exactly what a working client sends rather than what the notes say.
const (
	NegotiateByte byte = 0x12
	DirRequest    byte = 0xdc
	DirReply      byte = 0xdd
)

var magicClient = [4]byte{0xf0, 0xde, 0xbc, 0x0a}

var (
	ErrBadMagic    = errors.New("baichuan: bad magic")
	ErrShortHeader = errors.New("baichuan: short header")
)

// Header is a Baichuan message header. Bytes 16 and 17 carry different
// meanings by direction: a request sends a negotiation byte followed by 0xdc,
// a reply sends a status code followed by 0x00 or 0xdd.
type Header struct {
	MsgID      uint32
	MsgLen     uint32
	EncOffset  uint32
	EncByte    byte
	DirByte    byte
	Class      uint16
	PayloadOff uint32
}

// Status reads bytes 16 and 17 as the little endian status code a reply
// carries: 200 for success, 400 for a request the camera could not parse.
// On a request these bytes mean something else, so this is only meaningful
// on a message that came from a camera.
func (h Header) Status() int16 {
	return int16(uint16(h.EncByte) | uint16(h.DirByte)<<8)
}

// HeaderLen reports the header size for a class.
func HeaderLen(class uint16) int {
	switch class {
	case ClassModern24, ClassZero:
		return 24
	default:
		return 20
	}
}

// DecodeHeader parses a header from the front of b and reports its length.
func DecodeHeader(b []byte) (Header, int, error) {
	if len(b) < 20 {
		return Header{}, 0, ErrShortHeader
	}
	if b[0] != magicClient[0] || b[1] != magicClient[1] ||
		b[2] != magicClient[2] || b[3] != magicClient[3] {
		return Header{}, 0, ErrBadMagic
	}
	h := Header{
		MsgID:     binary.LittleEndian.Uint32(b[4:]),
		MsgLen:    binary.LittleEndian.Uint32(b[8:]),
		EncOffset: binary.LittleEndian.Uint32(b[12:]),
		EncByte:   b[16],
		DirByte:   b[17],
		Class:     binary.LittleEndian.Uint16(b[18:]),
	}
	n := HeaderLen(h.Class)
	if len(b) < n {
		return Header{}, 0, ErrShortHeader
	}
	if n == 24 {
		h.PayloadOff = binary.LittleEndian.Uint32(b[20:])
	}
	return h, n, nil
}

// Encode renders the header back to bytes.
func (h Header) Encode() []byte {
	n := HeaderLen(h.Class)
	b := make([]byte, n)
	copy(b, magicClient[:])
	binary.LittleEndian.PutUint32(b[4:], h.MsgID)
	binary.LittleEndian.PutUint32(b[8:], h.MsgLen)
	binary.LittleEndian.PutUint32(b[12:], h.EncOffset)
	b[16] = h.EncByte
	b[17] = h.DirByte
	binary.LittleEndian.PutUint16(b[18:], h.Class)
	if n == 24 {
		binary.LittleEndian.PutUint32(b[20:], h.PayloadOff)
	}
	return b
}

// EncOffsetFor builds the encryption-offset field. Observed on the wire as
// [channel, stream, counter, handle] little-endian, where the counter
// increments once per outbound request.
func EncOffsetFor(channel, stream, counter, handle byte) uint32 {
	return binary.LittleEndian.Uint32([]byte{channel, stream, counter, handle})
}
