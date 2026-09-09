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
}

// Reader reads framed messages from a stream.
//
// The camera sends messages back to back on one TCP connection and a message
// may be split across packets, so each read takes exactly the header and then
// exactly MsgLen bytes.
type Reader struct {
	br     *bufio.Reader
	aesKey []byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// SetAESKey switches body decryption to AES. Call it once login completes:
// the handshake uses the BC cipher, everything after it uses AES.
func (r *Reader) SetAESKey(k []byte) { r.aesKey = k }

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
		// tested and is the safer guess: resynchronising costs one packet,
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
	if r.aesKey == nil || !ok {
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
	if r.aesKey != nil {
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
	aesKey []byte
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// SetAESKey switches body encryption to AES, mirroring Reader.SetAESKey.
func (w *Writer) SetAESKey(k []byte) { w.aesKey = k }

// Write frames and sends one message. MsgLen is set from the encrypted body.
func (w *Writer) Write(h Header, body []byte) error {
	out := body
	if len(body) > 0 && h.Class != ClassLegacy {
		if w.aesKey != nil {
			enc, err := AESEncrypt(w.aesKey, body)
			if err != nil {
				return err
			}
			out = enc
		} else {
			out = BCCrypt(h.EncOffset, body)
		}
	}
	h.MsgLen = uint32(len(out))
	if _, err := w.w.Write(h.Encode()); err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}
	_, err := w.w.Write(out)
	return err
}
