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

// A media packet carries camera metadata between its header and the first
// NAL start code: 112 bytes on some HEVC frames, 80 on others, and 104 to
// 184 on the fisheye's H.264. The packet's size field does not count it, so
// the depacketiser skips it and every frame it emits now begins at a NAL
// boundary. This test holds that line: a nonzero offset here means the
// prefix is being handed to the decoder again, and, since the prefix is what
// makes a packet longer than hdr+size, that the tail of every frame is being
// dropped with it. See docs/protocol.md.
func TestFramesBeginAtANALBoundary(t *testing.T) {
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
	if len(offsets) == 0 {
		t.Fatal("no video frames decoded")
	}
	for off, n := range offsets {
		if off != 0 {
			t.Errorf("%d frames start %d bytes before their first NAL start code", n, off)
		}
	}
}
