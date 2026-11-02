package ts

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A TrackMix WiFi labels its recorded frames H264 while they hold H.265
// (#21); the picture's own first NAL unit decides.
func TestRecordedCodecReadsThePicture(t *testing.T) {
	for _, c := range []struct {
		label string
		data  []byte
		want  string
	}{
		{"H264", []byte{0, 0, 0, 1, 0x40, 0x01, 0x0c}, "H265"}, // VPS
		{"H264", []byte{0, 0, 1, 0x26, 0x01, 0xaf}, "H265"},    // IDR_W_RADL
		{"H265", []byte{0, 0, 0, 1, 0x67, 0x64, 0x00}, "H264"}, // SPS
		{"H264", []byte{0, 0, 0, 1, 0x65, 0x88}, "H264"},       // IDR
		{"H265", []byte{0xff, 0xff}, "H265"},                   // nothing to read: the label stands
	} {
		if got := recordedCodec(baichuan.Frame{Kind: baichuan.FrameIFrame, Codec: c.label, Data: c.data}); got != c.want {
			t.Errorf("%s % x: got %s, want %s", c.label, c.data, got, c.want)
		}
	}
}
