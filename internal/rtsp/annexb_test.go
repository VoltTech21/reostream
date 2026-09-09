package rtsp

import (
	"bytes"
	"testing"
)

func TestSplitAnnexB(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{
			"four byte start codes",
			[]byte{0, 0, 0, 1, 0x40, 0x01, 0, 0, 0, 1, 0x42, 0x02},
			[][]byte{{0x40, 0x01}, {0x42, 0x02}},
		},
		{
			"three byte start codes",
			[]byte{0, 0, 1, 0xaa, 0, 0, 1, 0xbb},
			[][]byte{{0xaa}, {0xbb}},
		},
		{
			"mixed lengths",
			[]byte{0, 0, 0, 1, 0x11, 0, 0, 1, 0x22, 0, 0, 0, 1, 0x33},
			[][]byte{{0x11}, {0x22}, {0x33}},
		},
		{
			"leading bytes before the first start code are skipped",
			[]byte{0x99, 0x98, 0, 0, 0, 1, 0x11},
			[][]byte{{0x11}},
		},
		{"empty", nil, nil},
		{"no start code at all", []byte{1, 2, 3}, nil},
		{
			// A start code with nothing after it must not produce an empty
			// NALU, which would be encoded as a zero length RTP payload.
			"trailing start code",
			[]byte{0, 0, 0, 1, 0x11, 0, 0, 0, 1},
			[][]byte{{0x11}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitAnnexB(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d NALUs, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if !bytes.Equal(got[i], tc.want[i]) {
					t.Errorf("NALU %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A real keyframe off a camera carries its parameter sets ahead of the slice,
// which is the whole reason this package splits the stream at all.
func TestSplitAnnexBOnRealKeyframe(t *testing.T) {
	nalus := splitAnnexB(h265Keyframe(t))
	if len(nalus) < 4 {
		t.Fatalf("got %d NALUs from a real keyframe, want VPS, SPS, PPS and a slice", len(nalus))
	}
	var sawVPS, sawSPS, sawPPS bool
	for _, n := range nalus {
		switch h265NALUType(n) {
		case h265VPS:
			sawVPS = true
		case h265SPS:
			sawSPS = true
		case h265PPS:
			sawPPS = true
		}
	}
	if !sawVPS || !sawSPS || !sawPPS {
		t.Errorf("vps=%v sps=%v pps=%v, want all three", sawVPS, sawSPS, sawPPS)
	}
}
