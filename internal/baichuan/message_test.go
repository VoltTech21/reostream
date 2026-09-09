package baichuan

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// Walking the real camera-to-client stream must yield the whole handshake and
// then media messages, with no desync.
func TestReaderWalksTheCapture(t *testing.T) {
	b := loadFixture(t, "stream_s2c.bin")
	r := NewReader(bytes.NewReader(b))
	var msgs, media int
	var sawNonce, sawDeviceInfo bool

	for i := 0; ; i++ {
		m, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break // the fixture is truncated mid-stream
		}
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		msgs++
		switch {
		case i == 0:
			if !strings.Contains(string(m.XML), fixtureNonce) {
				t.Errorf("first message should carry the nonce, got %.80q", m.XML)
			}
			sawNonce = true
		case i == 1:
			if !strings.Contains(string(m.XML), "<DeviceInfo") {
				t.Errorf("second message should be DeviceInfo, got %.80q", m.XML)
			}
			sawDeviceInfo = true
			// Login is complete: the camera switches to AES from here.
			r.SetAESKey(AESKey(fixtureNonce, fixturePassword))
		}
		if m.Header.MsgID == MsgIDVideo && len(m.Payload) > 0 {
			media++
		}
	}

	if !sawNonce || !sawDeviceInfo {
		t.Error("handshake messages missing")
	}
	if media == 0 {
		t.Error("no media payloads found")
	}
	t.Logf("walked %d messages, %d carrying media", msgs, media)
}

// After login the reader must decrypt with AES, not the BC cipher.
func TestReaderSwitchesToAES(t *testing.T) {
	b := loadFixture(t, "login_s2c.bin")
	r := NewReader(bytes.NewReader(b))
	for i := 0; i < 2; i++ {
		if _, err := r.Next(); err != nil {
			t.Fatalf("handshake message %d: %v", i, err)
		}
	}
	r.SetAESKey(AESKey(fixtureNonce, fixturePassword))
	m, err := r.Next()
	if err != nil {
		t.Fatalf("post-login message: %v", err)
	}
	if m.Header.MsgID != MsgIDAbilityInfo {
		t.Skipf("expected AbilityInfo, got id %d", m.Header.MsgID)
	}
	if !strings.Contains(string(m.XML), "<AbilityInfo") {
		t.Errorf("AbilityInfo did not decrypt with AES: %.80q", m.XML)
	}
}

// A header declaring a body bigger than MaxMessageSize must be rejected
// before the body is allocated, not after a failed read.
func TestReaderRejectsOversizedMsgLen(t *testing.T) {
	h := Header{MsgID: MsgIDVideo, Class: ClassModern24, MsgLen: MaxMessageSize + 1}
	r := NewReader(bytes.NewReader(h.Encode()))

	_, err := r.Next()
	if err == nil {
		t.Fatal("want an error for a header declaring an oversized MsgLen")
	}
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("error = %v, want ErrMessageTooLarge", err)
	}
}

// A PayloadOff pointing past the end of the body must be rejected with an
// error, not sliced (which would panic, and would go negative first on a
// 32-bit build if PayloadOff were converted to int before the check).
func TestReaderRejectsPayloadOffPastBody(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><body/>`)
	h := Header{
		MsgID:      MsgIDAbilityInfo,
		Class:      ClassModern24,
		MsgLen:     uint32(len(body)),
		PayloadOff: uint32(len(body)) + 1,
	}

	var buf bytes.Buffer
	buf.Write(h.Encode())
	buf.Write(body)
	r := NewReader(&buf)

	_, err := r.Next()
	if err == nil {
		t.Fatal("want an error for a PayloadOff past the body")
	}
	if !errors.Is(err, ErrPayloadOffsetOutOfRange) {
		t.Errorf("error = %v, want ErrPayloadOffsetOutOfRange", err)
	}
}

func TestWriterRoundTrips(t *testing.T) {
	h := Header{MsgID: MsgIDVideo, Class: ClassModern24, EncOffset: EncOffsetFor(0, 1, 2, 0)}
	body := []byte(`<?xml version="1.0"?><body/>`)

	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(h, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := NewReader(&buf).Next()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if m.Header.MsgID != MsgIDVideo {
		t.Errorf("MsgID = %d, want %d", m.Header.MsgID, MsgIDVideo)
	}
	if !bytes.Equal(m.XML, body) {
		t.Errorf("XML = %q, want %q", m.XML, body)
	}
}

func TestWriterRoundTripsUnderAES(t *testing.T) {
	key := AESKey(fixtureNonce, fixturePassword)
	h := Header{MsgID: MsgIDAbilityInfo, Class: ClassModern24}
	body := []byte(`<?xml version="1.0"?><body><x>1</x></body>`)

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetAESKey(key)
	if err := w.Write(h, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := NewReader(&buf)
	r.SetAESKey(key)
	m, err := r.Next()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(m.XML, body) {
		t.Errorf("XML = %q, want %q", m.XML, body)
	}
}

// The fisheye and the pano put an extension header on every message of a
// media packet, not just the first: continuations carry <checkPos> and
// <checkValue> with no <binaryData>. Reading an extension as a packet
// boundary made every message look like a fresh packet, so a keyframe
// spanning several hundred messages was discarded on each one and those two
// cameras delivered zero frames while the connection stayed busy.
func TestStartsPacketFollowsBinaryData(t *testing.T) {
	const hdr = `<?xml version="1.0" encoding="UTF-8" ?>`
	tests := []struct {
		name string
		xml  string
		want bool
	}{
		{"packet start", hdr + `<Extension version="1.1"><binaryData>1</binaryData></Extension>`, true},
		{"packet start with encryptLen", hdr + `<Extension version="1.1"><encryptLen>1024</encryptLen><binaryData>1</binaryData></Extension>`, true},
		{"fisheye continuation", hdr + `<Extension version="1.1"><checkPos>0</checkPos><checkValue>-234853339</checkValue></Extension>`, false},
		{"unparsable header falls back to a start", "not xml at all", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte("media bytes")
			// The reader decrypts the XML part before parsing it, so the
			// fixture has to be encrypted the way a camera would send it.
			body := append(BCCrypt(0, []byte(tt.xml)), payload...)
			h := Header{
				MsgID:      MsgIDVideo,
				Class:      ClassModern24,
				MsgLen:     uint32(len(body)),
				PayloadOff: uint32(len(tt.xml)),
			}

			var buf bytes.Buffer
			buf.Write(h.Encode())
			buf.Write(body)
			r := NewReader(&buf)

			m, err := r.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if m.StartsPacket != tt.want {
				t.Errorf("StartsPacket = %v, want %v", m.StartsPacket, tt.want)
			}
			if string(m.Payload) != string(payload) {
				t.Errorf("Payload = %q, want %q", m.Payload, payload)
			}
		})
	}
}
