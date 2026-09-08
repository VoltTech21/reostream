package baichuan

import (
	"bytes"
	"strings"
	"testing"
)

// The nonce the camera issued in the committed capture, and its (empty) password.
const (
	fixtureNonce    = "6a9b50e3-07t2qQdtEzXpGmKi9iW7"
	fixturePassword = ""
)

func TestBCCryptIsItsOwnInverse(t *testing.T) {
	plain := []byte(`<?xml version="1.0" encoding="UTF-8" ?><body></body>`)
	for _, off := range []uint32{0, 1, 7, 8, 0x00020100, 0x9cdb08} {
		enc := BCCrypt(off, plain)
		if bytes.Equal(enc, plain) {
			t.Errorf("offset %#x: ciphertext equals plaintext", off)
		}
		if got := BCCrypt(off, enc); !bytes.Equal(got, plain) {
			t.Errorf("offset %#x: round trip gave %q", off, got)
		}
	}
}

// walkCapture yields each message in a fixture.
func walkCapture(t *testing.T, name string, fn func(i int, h Header, body []byte) bool) {
	t.Helper()
	b := loadFixture(t, name)
	off := 0
	for i := 0; off+20 <= len(b); i++ {
		h, n, err := DecodeHeader(b[off:])
		if err != nil {
			return
		}
		end := off + n + int(h.MsgLen)
		if end > len(b) {
			return
		}
		if !fn(i, h, b[off+n:end]) {
			return
		}
		off = end
	}
}

// The camera's second message carries the login nonce, BC-encrypted.
func TestBCCryptDecryptsTheNonceReply(t *testing.T) {
	found := false
	walkCapture(t, "login_s2c.bin", func(i int, h Header, body []byte) bool {
		if i != 0 {
			return false
		}
		dec := string(BCCrypt(h.EncOffset, body))
		if !strings.Contains(dec, "<?xml") {
			t.Errorf("first reply did not decrypt to XML: %q", dec[:min(80, len(dec))])
			return false
		}
		if !strings.Contains(dec, fixtureNonce) {
			t.Errorf("nonce %q not found in:\n%s", fixtureNonce, dec)
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("no messages walked")
	}
}

// Everything after the login reply is AES, keyed by the nonce and password.
func TestAESDecryptsTheAbilityInfoReply(t *testing.T) {
	key := AESKey(fixtureNonce, fixturePassword)
	if got := string(key); got != "BEACA6E07B80E4D5" {
		t.Fatalf("key = %q, want BEACA6E07B80E4D5 (verified against the capture)", got)
	}
	found := false
	walkCapture(t, "login_s2c.bin", func(i int, h Header, body []byte) bool {
		if h.MsgID != MsgIDAbilityInfo || len(body) == 0 {
			return true
		}
		dec, err := AESDecrypt(key, body)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !strings.Contains(string(dec), "<AbilityInfo") {
			t.Errorf("AbilityInfo did not decrypt:\n%q", string(dec[:min(120, len(dec))]))
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("no AbilityInfo message in the capture")
	}
}

func TestAESRoundTrip(t *testing.T) {
	key := AESKey("nonce123", "secret")
	plain := []byte(`<?xml version="1.0"?><body/>`)
	enc, err := AESEncrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := AESDecrypt(key, enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(dec, plain) {
		t.Errorf("round trip gave %q, want %q", dec, plain)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
