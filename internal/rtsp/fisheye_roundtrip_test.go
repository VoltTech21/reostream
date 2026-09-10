package rtsp

import (
	"bytes"
	"os"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
)

// TestFisheyeRoundTrip runs a real captured fisheye keyframe access unit
// through the exact production path: splitAnnexB -> rtph264 encoder ->
// rtph264 decoder, and checks the reassembled NALUs match what went in.
// The fisheye IDR slice is ~1MB, so it exercises FU-A fragmentation across
// ~700 packets. If the RTP layer corrupts the picture, it shows up here with
// no camera, no network, no production restart.
func TestFisheyeRoundTrip(t *testing.T) {
	au, err := os.ReadFile("testdata/fe_keyframe_au.bin")
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}

	in := splitAnnexB(au)
	t.Logf("split into %d NALUs", len(in))
	for i, n := range in {
		t.Logf("  NALU[%d] type=%d len=%d", i, h264NALUType(n), len(n))
	}

	// Mirror production: H264 format, PacketizationMode 1, its encoder.
	f := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	enc, err := f.CreateEncoder()
	if err != nil {
		t.Fatalf("CreateEncoder: %v", err)
	}
	pkts, err := enc.Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	t.Logf("encoded into %d RTP packets", len(pkts))

	dec := &rtph264.Decoder{}
	if err := dec.Init(); err != nil {
		t.Fatalf("dec.Init: %v", err)
	}

	var got [][]byte
	for i, p := range pkts {
		nalus, err := dec.Decode(p)
		if err != nil {
			// ErrMorePacketsNeeded is expected mid-FU-A; anything else is a bug.
			if err.Error() == "need more packets" {
				continue
			}
			t.Logf("packet %d/%d seq=%d marker=%v decode err: %v", i, len(pkts), p.SequenceNumber, p.Marker, err)
			continue
		}
		got = append(got, nalus...)
	}
	t.Logf("decoded back into %d NALUs", len(got))

	if len(got) != len(in) {
		t.Errorf("NALU count mismatch: in=%d out=%d", len(in), len(got))
	}
	n := len(got)
	if len(in) < n {
		n = len(in)
	}
	for i := 0; i < n; i++ {
		if !bytes.Equal(in[i], got[i]) {
			t.Errorf("NALU[%d] mismatch: in type=%d len=%d, out type=%d len=%d",
				i, h264NALUType(in[i]), len(in[i]), h264NALUType(got[i]), len(got[i]))
		}
	}
}
