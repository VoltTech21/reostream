package baichuan

import (
	"bytes"
	"os"
	"testing"
)

var clientMagic = []byte{0xf0, 0xde, 0xbc, 0x0a}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if len(b) == 0 {
		t.Fatalf("fixture %s is empty", name)
	}
	return b
}

func TestFixturesStartWithClientMagic(t *testing.T) {
	for _, name := range []string{"login_c2s.bin", "login_s2c.bin"} {
		b := loadFixture(t, name)
		if !bytes.HasPrefix(b, clientMagic) {
			t.Errorf("%s: want magic %x, got %x", name, clientMagic, b[:4])
		}
	}
}

// The first client message is a zero-length encryption-negotiation probe.
func TestDecodeHeaderFromFixture(t *testing.T) {
	b := loadFixture(t, "login_c2s.bin")
	h, n, err := DecodeHeader(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.MsgID != MsgIDLogin {
		t.Errorf("MsgID = %d, want %d", h.MsgID, MsgIDLogin)
	}
	if h.Class != ClassLegacy {
		t.Errorf("Class = %#x, want %#x", h.Class, ClassLegacy)
	}
	if n != 20 {
		t.Errorf("header length = %d, want 20", n)
	}
	if h.MsgLen != 0 {
		t.Errorf("MsgLen = %d, want 0 for the probe", h.MsgLen)
	}
	if h.EncByte != NegotiateByte || h.DirByte != DirRequest {
		t.Errorf("negotiation bytes = %#x/%#x, want %#x/%#x",
			h.EncByte, h.DirByte, NegotiateByte, DirRequest)
	}
}

func TestHeaderRoundTripAgainstCapture(t *testing.T) {
	b := loadFixture(t, "login_c2s.bin")
	off := 0
	for i := 0; i < 4; i++ {
		h, n, err := DecodeHeader(b[off:])
		if err != nil {
			t.Fatalf("message %d: decode: %v", i, err)
		}
		got := h.Encode()
		if !bytes.Equal(got, b[off:off+n]) {
			t.Fatalf("message %d: encoded %x, captured %x", i, got, b[off:off+n])
		}
		off += n + int(h.MsgLen)
	}
}

func TestEncOffsetFor(t *testing.T) {
	// The capture's third request used counter 2 on stream 1.
	if got := EncOffsetFor(0, 1, 2, 0); got != 0x00020100 {
		t.Errorf("EncOffsetFor = %#010x, want 0x00020100", got)
	}
}

func TestDecodeHeaderRejectsBadInput(t *testing.T) {
	if _, _, err := DecodeHeader(make([]byte, 20)); err == nil {
		t.Error("want an error for a zero header")
	}
	if _, _, err := DecodeHeader([]byte{0xf0, 0xde}); err == nil {
		t.Error("want an error for a truncated header")
	}
}
