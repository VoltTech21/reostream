package baichuan

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Real capture: the pano's proprietary prefix metadata sometimes contains a
// false 00 00 00 01 followed by a few bytes before the true first NAL. The
// old prefix detection stopped at that false start code, so the frame Data
// began with a garbage reserved-type NAL (and the tail got truncated). These
// are the exact garbage headers seen on the wire: 15 22, 34 c8, ad ed, b5 17.
func TestPFrameSkipsFalseStartCodeInPrefix(t *testing.T) {
	picture := append([]byte{0, 0, 0, 1, 0x02, 0x01}, bytes.Repeat([]byte{0x88}, 40)...) // real TRAIL slice (type 1)
	for _, garbage := range [][]byte{{0x15, 0x22, 0xd4}, {0x34, 0xc8, 0x30}, {0xad, 0xed, 0x78}, {0xb5, 0x17, 0xc5}} {
		meta := bytes.Repeat([]byte{0xAA}, 20)             // proprietary metadata, no start code
		falseNAL := append([]byte{0, 0, 0, 1}, garbage...) // false start code + garbage
		body := append(append(meta, falseNAL...), picture...)

		buf := make([]byte, 24)
		copy(buf[0:4], magicPFrame)
		copy(buf[4:8], []byte("H265"))
		binary.LittleEndian.PutUint32(buf[8:12], uint32(len(picture))) // size = real picture only
		buf = append(buf, body...)

		var d Depacketiser
		d.Write(buf, true)
		f, ok := d.Next()
		if !ok {
			t.Fatalf("garbage %x: no frame", garbage)
		}
		v := f.Video()
		// first NAL of the emitted frame must be the real slice, not garbage
		if !bytes.HasPrefix(v, []byte{0, 0, 0, 1, 0x02, 0x01}) {
			t.Errorf("garbage %x: frame starts with %x, want the real slice 00000001 0201", garbage, v[:min(8, len(v))])
		}
		// and it must be the whole picture, not truncated
		if len(v) != len(picture) {
			t.Errorf("garbage %x: got %d bytes, want %d (tail truncated?)", garbage, len(v), len(picture))
		}
	}
}

// A normal HEVC frame whose first start code already begins a valid NAL must
// be unchanged by the new logic.
func TestPFrameNormalPrefixUnchanged(t *testing.T) {
	picture := append([]byte{0, 0, 0, 1, 0x26, 0x01}, bytes.Repeat([]byte{0x77}, 30)...) // IDR-ish valid NAL type 19
	meta := bytes.Repeat([]byte{0xAA}, 12)
	body := append(meta, picture...)
	buf := make([]byte, 24)
	copy(buf[0:4], magicIFrame)
	copy(buf[4:8], []byte("H265"))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(picture)))
	buf = append(buf, body...)
	// I-frame header is 32 bytes, extend
	buf = append(buf[:24], append(make([]byte, 8), body...)...)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(picture)))

	var d Depacketiser
	d.Write(buf, true)
	f, ok := d.Next()
	if !ok {
		t.Fatal("no frame")
	}
	if !bytes.HasPrefix(f.Video(), []byte{0, 0, 0, 1, 0x26, 0x01}) {
		t.Errorf("normal frame altered: %x", f.Video()[:min(8, len(f.Video()))])
	}
}
