package baichuan

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The bytes below were cut out of live captures of the fisheye, the only
// H.264 camera in the fleet. Each one is prefix metadata that followed a
// false 00 00 00 01 and passed the original "forbidden bit clear, type
// 1-23" check, so the prefix search stopped on it and published the
// metadata as the first bytes of the picture.
//
// What it cost: ffmpeg logged "non-existing PPS 1 referenced" and
// "log2_max_frame_num_minus4 out of range (0-12)", and Frigate discarded
// the recording segment that began on one of these as "Invalid or missing
// video stream". One corrupt NAL per 30 to 120 seconds, on this camera
// only; the HEVC cameras were clean because their check was already strict.
var capturedPrefixJunk = [][]byte{
	// nal_unit_type 2, slice data partition A, which no Reolink camera emits.
	{0x42, 0x98, 0xa7, 0xfb, 0x09, 0x00, 0x01, 0x01, 0x00, 0x01, 0x02, 0x02, 0x00, 0xf5, 0x04},
	// nal_unit_type 7 (SPS) with profile_idc 123, which the spec does not define.
	{0x27, 0x7b, 0xc7, 0xfb, 0x09, 0x00, 0x01, 0x01, 0x00, 0x01, 0x02, 0x02, 0x00, 0xf5, 0x04},
}

func TestH264PrefixJunkFromRealCapturesIsRejected(t *testing.T) {
	for _, junk := range capturedPrefixJunk {
		if h264NALHeaderValid(junk) {
			t.Errorf("%x accepted as a NAL header; it is camera prefix metadata", junk[:4])
		}
	}
}

// The other half of the property: the headers the camera really does send
// must still be accepted, or tightening the check would trade a rare
// corruption for a stream that never starts.
func TestRealH264NALHeadersAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
	}{
		// Taken from the same captures.
		{"SPS, profile 100 level 51", []byte{0x67, 0x64, 0x00, 0x33, 0xac}},
		{"PPS", []byte{0x68, 0xee, 0x3c, 0xb0}},
		{"IDR slice", []byte{0x65, 0x88, 0x84, 0x00}},
		{"non-IDR slice", []byte{0x41, 0x9a, 0x01, 0x40}},
		{"non-IDR disposable", []byte{0x01, 0x9a, 0x01, 0x40}},
		{"SEI", []byte{0x06, 0x05, 0x10, 0x00}},
		{"access unit delimiter", []byte{0x09, 0x10}},
	}
	for _, c := range cases {
		if !h264NALHeaderValid(c.b) {
			t.Errorf("%s (%x) rejected; the camera sends these", c.name, c.b)
		}
	}
}

// End to end through the depacketiser: a frame whose prefix carries the
// captured junk must still yield the real picture, whole.
func TestFrameWithCapturedJunkInPrefixYieldsTheWholePicture(t *testing.T) {
	picture := append([]byte{0, 0, 0, 1, 0x41, 0x9a}, bytes.Repeat([]byte{0x88}, 64)...)

	for _, junk := range capturedPrefixJunk {
		meta := bytes.Repeat([]byte{0xCC}, 24)
		body := append(append(append(meta, 0, 0, 0, 1), junk...), picture...)

		buf := make([]byte, 24)
		copy(buf[0:4], magicPFrame)
		copy(buf[4:8], []byte("H264"))
		binary.LittleEndian.PutUint32(buf[8:12], uint32(len(picture)))
		buf = append(buf, body...)

		var d Depacketiser
		d.Write(buf, true)
		f, ok := d.Next()
		if !ok {
			t.Fatalf("junk %x: no frame came out", junk[:4])
		}
		v := f.Video()
		if !bytes.Equal(v, picture) {
			t.Errorf("junk %x: got %d bytes starting %x, want the %d byte picture starting %x",
				junk[:4], len(v), v[:min(6, len(v))], len(picture), picture[:6])
		}
	}
}
