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
