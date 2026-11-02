package baichuan

import (
	"bufio"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

// MaxMessageSize bounds a single message body.
//
// h.MsgLen comes straight off the wire, before the body is allocated, so a
// malfunctioning or hostile peer declaring a huge length must be rejected
// before make() runs rather than after. The bound is the size of one
// uncompressed 4K frame: docs/measurements.md records the main stream at
// 3840x2160, and no message on this protocol carries more than one frame's
// worth of compressed data, which can never exceed the raw pixel data it
// encodes. 3840 * 2160 * 3 / 2 (YUV 4:2:0, the format the sensor captures
// before encoding) = 12,441,600 bytes.
const MaxMessageSize = 3840 * 2160 * 3 / 2

// ErrMessageTooLarge means a header declared a body bigger than
// MaxMessageSize.
var ErrMessageTooLarge = errors.New("baichuan: message body exceeds MaxMessageSize")

// ErrPayloadOffsetOutOfRange means a header's PayloadOff pointed past the end
// of the body it belongs to.
var ErrPayloadOffsetOutOfRange = errors.New("baichuan: PayloadOff exceeds body length")

// Message is a decoded Baichuan message. XML is decrypted; Payload is the
// binary remainder, which is media data and is never encrypted.
type Message struct {
	Header  Header
	XML     []byte
	Payload []byte

	// StartsPacket reports that this message begins a new media packet.
	//
	// A media packet always starts at the beginning of a message carrying an
	// extension header, and continues through following messages that carry
	// none. Any bytes between the packet's declared end and the end of its
	// final message are filler and must be discarded.
	StartsPacket bool

	// PayloadXML and WholeXML are the other two ways a config reply that
	// came with a payload section can be read, filled only for messages
	// that are not video. PayloadXML is the payload decrypted on its own,
	// the layout neolink parses: an extension, then the document as a
	// separately encrypted payload. WholeXML is the entire body decrypted
	// as one, for a camera whose PayloadOff falls inside the document. No
	// camera here sends either, so they exist for a caller to fall back on
	// when XML does not parse, and to describe the reply when none do.
	PayloadXML []byte
	WholeXML   []byte
}

// Cipher is how message bodies are sealed once login completes.
//
// The camera picks it: the negotiation probe asks for full AES, and the
// reply names what the camera will actually use in byte 16, with DirReply in
// byte 17. Current firmware answers 0x12 and the v3.0.0.136 MIPS builds
// answer 0x02, both AES. Firmware from 2017-2019 (C1 Pro, RLC-410 on
// v2.0.0.x) answers 0x01 and keeps the BC cipher for the whole session, and
// a Preview sealed with AES is something it cannot read, so it never answers
// and the stream looks like a held session (#23).
type Cipher int

const (
	// CipherBC is the XOR cipher every session starts with, and the zero
	// value, so a fresh Reader or Writer can read and write the handshake.
	CipherBC Cipher = iota
	CipherAES
	CipherNone
)

func (c Cipher) String() string {
	switch c {
	case CipherAES:
		return "aes"
	case CipherNone:
		return "none"
	}
	return "bc"
}

// negotiatedCipher reads the cipher a camera chose from its negotiation
// reply. Anything it does not recognize, including a reply that does not
// mark byte 17 as DirReply, means AES, which is what every session used
// before the byte was read, so a camera that says something unexpected keeps
// working exactly as it did.
func negotiatedCipher(h Header) Cipher {
	if h.DirByte != DirReply {
		return CipherAES
	}
	switch h.EncByte {
	case 0x00:
		return CipherNone
	case 0x01:
		return CipherBC
	}
	return CipherAES
}

// Reader reads framed messages from a stream.
//
// The camera sends messages back to back on one TCP connection and a message
// may be split across packets, so each read takes exactly the header and then
// exactly MsgLen bytes.
type Reader struct {
	br     *bufio.Reader
	cipher Cipher
	aesKey []byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// SetAESKey switches body decryption to AES. Call it once login completes:
// the handshake uses the BC cipher, everything after it uses AES.
func (r *Reader) SetAESKey(k []byte) { r.SetCipher(CipherAES, k) }

// SetCipher switches body decryption to the cipher the camera chose during
// login. k is the session's AES key and only matters for CipherAES.
func (r *Reader) SetCipher(c Cipher, k []byte) { r.cipher, r.aesKey = c, k }

// Next reads one complete message, blocking until all of it has arrived.
func (r *Reader) Next() (Message, error) {
	head := make([]byte, 24)
	if _, err := io.ReadFull(r.br, head[:20]); err != nil {
		return Message{}, err
	}
	// The class lives at bytes 18..20 and decides the header length, so it has
	// to be read before the header can be parsed as a whole.
	n := HeaderLen(binary.LittleEndian.Uint16(head[18:]))
	if n > 20 {
		if _, err := io.ReadFull(r.br, head[20:n]); err != nil {
			return Message{}, err
		}
	}
	h, _, err := DecodeHeader(head[:n])
	if err != nil {
		return Message{}, err
	}
	if h.MsgLen > MaxMessageSize {
		return Message{}, fmt.Errorf("baichuan: msg %d: length %d: %w", h.MsgID, h.MsgLen, ErrMessageTooLarge)
	}
	body := make([]byte, h.MsgLen)
	if _, err := io.ReadFull(r.br, body); err != nil {
		return Message{}, fmt.Errorf("baichuan: body of msg %d: %w", h.MsgID, err)
	}

	m := Message{Header: h}
	if len(body) == 0 {
		return m, nil
	}

	// On a 24-byte class, PayloadOff marks the end of the extension XML and
	// the start of the binary payload.
	//
	// A zero PayloadOff means "all payload" only for the camera's own media
	// messages, which arrive as class 0x0000. A client Preview *request* is
	// also message id 3 with PayloadOff 0, but arrives as class 0x6414 and
	// carries XML, so the class is what separates the two.
	if h.Class == ClassZero && h.PayloadOff == 0 && h.MsgID == MsgIDVideo {
		m.Payload = body
		return m, nil
	}
	xmlEnd := len(body)
	if HeaderLen(h.Class) == 24 && h.PayloadOff > 0 {
		// PayloadOff is wire-controlled: compare as uint32 against the body
		// length before ever converting to int, so a value that would go
		// negative on a 32-bit int (or simply run past the body) is caught
		// here instead of panicking on the slice below or on a 32-bit build.
		if h.PayloadOff > uint32(len(body)) {
			return Message{}, fmt.Errorf("baichuan: msg %d: PayloadOff %d exceeds body length %d: %w",
				h.MsgID, h.PayloadOff, len(body), ErrPayloadOffsetOutOfRange)
		}
		xmlEnd = int(h.PayloadOff)
	}
	m.XML = r.decrypt(h, body[:xmlEnd])
	if xmlEnd < len(body) {
		m.Payload = body[xmlEnd:]
		if h.MsgID != MsgIDVideo {
			m.PayloadXML = r.decrypt(h, body[xmlEnd:])
			m.WholeXML = r.decrypt(h, body)
		}
		ext, ok := parseExtension(m.XML)
		// An extension header is not by itself a packet boundary. Most
		// cameras send one only on the message that begins a packet and
		// nothing at all on the continuations, but the 2560x2560 fisheye and
		// the dual lens pano put an extension carrying only <checkPos> and
		// <checkValue> on every continuation message as well. Treating those
		// as packet starts discarded the partial frame on every message, so
		// a keyframe spanning several hundred messages never completed and
		// the stream delivered zero frames while looking healthy at the byte
		// level. <binaryData> is what actually marks a packet start: it is
		// present on the first message of every packet on every camera here,
		// and absent from every continuation.
		//
		// A header that fails to parse falls back to treating the message as
		// a packet start, which is what this did before the fisheye was
		// tested and is the safer guess: resynchronizing costs one packet,
		// while wrongly appending to the previous one corrupts it.
		m.StartsPacket = !ok || ext.BinaryData != 0
		r.decryptPayload(&m, ext, ok)
	}
	return m, nil
}

// extensionHeader is the small XML that precedes a binary payload.
type extensionHeader struct {
	XMLName    xml.Name `xml:"Extension"`
	BinaryData int      `xml:"binaryData"`
	EncryptLen int      `xml:"encryptLen"`
}

// parseExtension reads a media message's extension header, reporting false if
// there is none or it does not parse.
func parseExtension(x []byte) (extensionHeader, bool) {
	var ext extensionHeader
	if len(x) == 0 {
		return ext, false
	}
	if err := xml.Unmarshal(x, &ext); err != nil {
		return extensionHeader{}, false
	}
	return ext, true
}

// decryptPayload decrypts the leading, encrypted part of a media payload.
//
// A media message's extension XML may carry <encryptLen>N</encryptLen>, which
// means the first N bytes of that message's payload are AES-encrypted and the
// rest is plaintext. Messages with no extension at all are plaintext
// continuations of a packet an earlier message began. Handling it here keeps
// Payload always-plaintext for everything above.
func (r *Reader) decryptPayload(m *Message, ext extensionHeader, ok bool) {
	if r.cipher != CipherAES || !ok {
		return
	}
	n := ext.EncryptLen
	if n <= 0 {
		return
	}
	if n > len(m.Payload) {
		n = len(m.Payload)
	}
	dec, err := AESDecrypt(r.aesKey, m.Payload[:n])
	if err != nil {
		return
	}
	m.Payload = append(dec, m.Payload[n:]...)
}

func (r *Reader) decrypt(h Header, body []byte) []byte {
	if h.Class == ClassLegacy {
		return body // legacy bodies are fixed-width binary, not XML
	}
	switch r.cipher {
	case CipherNone:
		return body
	case CipherAES:
		if out, err := AESDecrypt(r.aesKey, body); err == nil {
			return out
		}
	}
	return BCCrypt(h.EncOffset, body)
}

// Writer writes framed messages, encrypting bodies to match what the camera
// expects at each stage of the session.
type Writer struct {
	w      io.Writer
	cipher Cipher
	aesKey []byte
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// SetAESKey switches body encryption to AES, mirroring Reader.SetAESKey.
func (w *Writer) SetAESKey(k []byte) { w.SetCipher(CipherAES, k) }

// SetCipher switches body encryption to c, mirroring Reader.SetCipher.
func (w *Writer) SetCipher(c Cipher, k []byte) { w.cipher, w.aesKey = c, k }

// Write frames and sends one message. MsgLen is set from the encrypted body.
// WriteParts writes a message that has an XML header and a binary section,
// sealing each as its own cipher stream.
//
// The two parts are sealed separately, both starting from the fixed IV, and
// that detail is the whole reason this exists. Write seals a body as one
// continuous stream, which is right for a message that is XML to the end and
// wrong for one that is not: a TalkConfig sent that way is refused with
// status 400, and so is one whose binary section is left in plaintext. Only
// separate streams are accepted.
//
// It mirrors the receive side, where a message's payload is decrypted as its
// own stream rather than as a continuation of the one covering the XML. The
// camera's parser is symmetric; ours has to be too.
func (w *Writer) WriteParts(h Header, xmlPart, binPart []byte) error {
	return w.writeParts(h, xmlPart, binPart, true)
}

// WriteMedia writes a message whose second section is media rather than a
// second XML document, so the section is left in plaintext.
//
// The rule is the extension header's own: a payload is encrypted only as far
// as <encryptLen> says, and a message that declares no encryptLen carries
// plaintext. That is how the camera's own audio and video arrive, and an
// outbound media message follows the same rule. A TalkConfig is the other
// case, since its second section is XML, and it is refused unless sealed.
func (w *Writer) WriteMedia(h Header, xmlPart, mediaPart []byte) error {
	return w.writeParts(h, xmlPart, mediaPart, false)
}

func (w *Writer) writeParts(h Header, xmlPart, binPart []byte, sealBin bool) error {
	out, err := w.seal(h, xmlPart)
	if err != nil {
		return err
	}
	if sealBin {
		if binPart, err = w.seal(h, binPart); err != nil {
			return err
		}
	}
	h.PayloadOff = uint32(len(out))
	h.MsgLen = uint32(len(out) + len(binPart))
	return w.send(h, out, binPart)
}

// send writes a whole message in one Write, so messages from concurrent
// writers never interleave on the wire.
func (w *Writer) send(h Header, parts ...[]byte) error {
	b := h.Encode()
	for _, p := range parts {
		b = append(b, p...)
	}
	_, err := w.w.Write(b)
	return err
}

// seal applies whichever cipher the connection is currently using.
func (w *Writer) seal(h Header, b []byte) ([]byte, error) {
	if len(b) == 0 || h.Class == ClassLegacy {
		return b, nil
	}
	switch w.cipher {
	case CipherNone:
		return b, nil
	case CipherAES:
		return AESEncrypt(w.aesKey, b)
	}
	return BCCrypt(h.EncOffset, b), nil
}

func (w *Writer) Write(h Header, body []byte) error {
	out, err := w.seal(h, body)
	if err != nil {
		return err
	}
	h.MsgLen = uint32(len(out))
	return w.send(h, out)
}
