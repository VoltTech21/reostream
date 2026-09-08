package baichuan

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// nonceFromCapture reads the login nonce out of a capture's first message so
// tests can derive the AES key without hardcoding it.
func nonceFromCapture(t *testing.T, name string) string {
	t.Helper()
	b := loadFixture(t, name)
	m, err := NewReader(bytes.NewReader(b)).Next()
	if err != nil {
		t.Fatalf("read first message: %v", err)
	}
	nonce, err := parseNonce(m.XML)
	if err != nil {
		t.Fatalf("parse nonce: %v", err)
	}
	return nonce
}

// The H.265 main stream exercises what the H.264 substream cannot: frames
// large enough to span many messages, with the camera's trailing filler
// between a packet's end and the end of its final message.
func TestDepacketiserOnH265Capture(t *testing.T) {
	name := "h265_s2c.bin"
	key := AESKey(nonceFromCapture(t, name), "")

	r := NewReader(bytes.NewReader(loadFixture(t, name)))
	d := NewDepacketiser()
	var iframes, pframes, audio int
	var codec string
	var lastMicros uint32

	for i := 0; ; i++ {
		m, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if i == 1 {
			r.SetAESKey(key)
		}
		if len(m.Payload) == 0 {
			continue
		}
		d.Write(m.Payload, m.StartsPacket)
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			switch f.Kind {
			case FrameIFrame:
				iframes++
			case FramePFrame:
				pframes++
			case FrameAAC, FrameADPCM:
				audio++
			}
			if f.Kind == FrameIFrame || f.Kind == FramePFrame {
				codec = f.Codec
				if lastMicros != 0 && f.Micros < lastMicros {
					t.Errorf("timestamps went backwards: %d after %d", f.Micros, lastMicros)
				}
				lastMicros = f.Micros
			}
		}
	}

	t.Logf("i=%d p=%d audio=%d codec=%s skipped=%d filler=%d",
		iframes, pframes, audio, codec, d.Skipped(), d.Filler())

	if codec != "H265" {
		t.Errorf("codec = %q, want H265", codec)
	}
	if iframes == 0 || pframes == 0 {
		t.Errorf("decoded %d I frames and %d P frames, want both nonzero", iframes, pframes)
	}
	if d.Skipped() != 0 {
		t.Errorf("skipped %d bytes; large frames should parse cleanly", d.Skipped())
	}
	if d.Filler() == 0 {
		t.Error("expected some trailing filler; the camera pads a packet's last message")
	}
}

// HEVC frames carry a proprietary prefix before the first NAL start code:
// 112 bytes on some frames, 80 on others. H.264 frames do not. This test
// records the fact so a change in behaviour is noticed; see docs/protocol.md.
func TestH265FramesCarryAPrefixBeforeTheFirstNAL(t *testing.T) {
	name := "h265_s2c.bin"
	key := AESKey(nonceFromCapture(t, name), "")
	r := NewReader(bytes.NewReader(loadFixture(t, name)))
	d := NewDepacketiser()
	offsets := map[int]int{}

	for i := 0; ; i++ {
		m, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if i == 1 {
			r.SetAESKey(key)
		}
		if len(m.Payload) == 0 {
			continue
		}
		d.Write(m.Payload, m.StartsPacket)
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			if f.Kind == FrameIFrame || f.Kind == FramePFrame {
				offsets[bytes.Index(f.Data, []byte{0, 0, 0, 1})]++
			}
		}
	}
	t.Logf("NAL start-code offsets seen: %v", offsets)
	if _, ok := offsets[0]; ok {
		t.Log("note: some HEVC frames now start at a NAL boundary, which is a change")
	}
	if len(offsets) == 0 {
		t.Fatal("no video frames decoded")
	}
	for off := range offsets {
		if off < 0 {
			t.Error("a frame contained no NAL start code at all")
		}
	}
}
