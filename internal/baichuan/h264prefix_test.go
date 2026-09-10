package baichuan

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The fisheye is H.264 and has the same proprietary prefix as the pano: a
// false 00 00 00 01 in the metadata made the naive prefix search stop early,
// leaking a garbage NAL and truncating the tail. Same bug the HEVC fix
// solved, but H.264 was never covered.
func TestPFrameH264SkipsFalseStartCodeInPrefix(t *testing.T) {
	// real H.264 slice header: 0x41 = non-IDR slice (type 1, ref_idc 2)
	picture := append([]byte{0, 0, 0, 1, 0x41, 0x9a}, bytes.Repeat([]byte{0x88}, 40)...)
	// garbage bytes that must be rejected as NAL headers:
	// 0xaa forbidden-bit set; 0x00 type 0 (unspecified); 0x1f type 31 (unspecified)
	for _, garbage := range [][]byte{{0xaa, 0x12, 0x34}, {0x00, 0x99, 0x01}, {0x1f, 0x22, 0x33}} {
		meta := bytes.Repeat([]byte{0xCC}, 18)
		falseNAL := append([]byte{0, 0, 0, 1}, garbage...)
		body := append(append(meta, falseNAL...), picture...)
		buf := make([]byte, 24)
		copy(buf[0:4], magicPFrame)
		copy(buf[4:8], []byte("H264"))
		binary.LittleEndian.PutUint32(buf[8:12], uint32(len(picture)))
		buf = append(buf, body...)

		var d Depacketiser
		d.Write(buf, true)
		f, ok := d.Next()
		if !ok {
			t.Fatalf("garbage %x: no frame", garbage)
		}
		v := f.Video()
		if !bytes.HasPrefix(v, []byte{0, 0, 0, 1, 0x41}) {
			t.Errorf("garbage %x: frame starts %x, want real slice 00000001 41", garbage, v[:min(8, len(v))])
		}
		if len(v) != len(picture) {
			t.Errorf("garbage %x: got %d bytes want %d (tail truncated?)", garbage, len(v), len(picture))
		}
	}
}
