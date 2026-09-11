package rtsp

import (
	"bytes"
	"os"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
)

// splitAUs breaks a continuous Annex-B stream into access units at each AUD
// (type 9), mirroring how the camera delimits frames. Returns one []byte per
// frame, each still Annex-B (with start codes), exactly what f.Video() yields
// to the RTSP path per frame.
func splitAUs(es []byte) [][]byte {
	nalus := splitAnnexB(es)
	var aus [][]byte
	var cur []byte
	sc := []byte{0, 0, 0, 1}
	for _, n := range nalus {
		cur = append(cur, sc...)
		cur = append(cur, n...)
		// This capture is single-slice-per-frame, so every VCL NAL (1/5)
		// ends its access unit. Preceding SPS/PPS attach to the AU.
		if t := h264NALUType(n); t == 1 || t == 5 {
			aus = append(aus, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		aus = append(aus, cur)
	}
	return aus
}

// TestFisheyeStreamRoundTrip replays a real multi-frame fisheye capture
// through the exact production path: per-frame splitAnnexB -> encoder.Encode
// -> production timestamp override -> a single long-lived decoder. If AU
// boundaries break (marker/timestamp), slices from adjacent frames merge and
// show as decode errors here, no camera or network needed.
func TestFisheyeStreamRoundTrip(t *testing.T) {
	es, err := os.ReadFile("testdata/fe_stream.h264")
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}
	aus := splitAUs(es)
	t.Logf("%d access units", len(aus))

	f := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	enc, err := f.CreateEncoder()
	if err != nil {
		t.Fatalf("CreateEncoder: %v", err)
	}
	dec := &rtph264.Decoder{}
	if err := dec.Init(); err != nil {
		t.Fatalf("dec.Init: %v", err)
	}

	var ts90 uint32 = 12345
	var decoded [][][]byte // decoded AUs, each a list of NALUs
	for _, au := range aus {
		in := splitAnnexB(au)
		pkts, err := enc.Encode(in)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		for _, p := range pkts {
			p.Timestamp = ts90 // production override
			out, err := dec.Decode(p)
			if err != nil {
				if err == rtph264.ErrMorePacketsNeeded || err == rtph264.ErrNonStartingPacketAndNoPrevious {
					continue
				}
				t.Errorf("decode err seq=%d ts=%d marker=%v: %v", p.SequenceNumber, p.Timestamp, p.Marker, err)
				continue
			}
			decoded = append(decoded, out)
		}
		ts90 += 3600 // 90000/25fps
	}
	t.Logf("decoded %d AUs from %d input AUs", len(decoded), len(aus))

	// Compare: input VCL NALUs vs decoded, AU by AU.
	if len(decoded) != len(aus) {
		t.Errorf("AU count mismatch: in=%d decoded=%d (frames merged or split)", len(aus), len(decoded))
	}
	mismatch := 0
	n := len(decoded)
	if len(aus) < n {
		n = len(aus)
	}
	for i := 0; i < n; i++ {
		in := splitAnnexB(aus[i])
		out := decoded[i]
		if len(in) != len(out) {
			mismatch++
			if mismatch <= 5 {
				t.Errorf("AU[%d] NALU count in=%d out=%d", i, len(in), len(out))
			}
			continue
		}
		for j := range in {
			if !bytes.Equal(in[j], out[j]) {
				mismatch++
				if mismatch <= 5 {
					t.Errorf("AU[%d] NALU[%d] bytes differ (in type=%d len=%d out type=%d len=%d)",
						i, j, h264NALUType(in[j]), len(in[j]), h264NALUType(out[j]), len(out[j]))
				}
				break
			}
		}
	}
	if mismatch == 0 {
		t.Logf("ALL %d frames byte-identical through the RTP layer", len(aus))
	} else {
		t.Errorf("%d frames corrupted", mismatch)
	}
}
