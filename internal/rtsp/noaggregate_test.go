package rtsp

import (
	"bytes"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
)

// An access unit of several small slices plus one too big for a packet:
// the shape of a Duo 2 P-frame (#25). Every NAL unit must leave in packets
// of its own, never an aggregation packet, with the marker only at the end.
func TestEncodeEachNeverAggregates(t *testing.T) {
	big := bytes.Repeat([]byte{0xaa}, 4000)

	h265 := &rtph265.Encoder{PayloadType: 96}
	if err := h265.Init(); err != nil {
		t.Fatal(err)
	}
	h264 := &rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
	if err := h264.Init(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		enc       videoEncoder
		au        [][]byte
		aggregate func(p []byte) bool
	}{
		{
			name: "h265",
			enc:  h265,
			// TRAIL_R slices: type 1 is (1<<1)=0x02 in the first header byte.
			au: [][]byte{{0x02, 0x01, 0x10}, {0x02, 0x01, 0x20}, {0x02, 0x01, 0x30}, append([]byte{0x02, 0x01}, big...)},
			aggregate: func(p []byte) bool {
				return (p[0]>>1)&0x3f == 48
			},
		},
		{
			name: "h264",
			enc:  h264,
			// Non-IDR slices: type 1.
			au: [][]byte{{0x41, 0x10}, {0x41, 0x20}, {0x41, 0x30}, append([]byte{0x41}, big...)},
			aggregate: func(p []byte) bool {
				t := p[0] & 0x1f
				return t >= 24 && t <= 27
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pkts, err := encodeEach(c.enc, c.au)
			if err != nil {
				t.Fatal(err)
			}
			// 3 single packets plus at least 3 fragments for 4000 bytes.
			if len(pkts) < 6 {
				t.Fatalf("got %d packets, want at least 6", len(pkts))
			}
			for i, p := range pkts {
				if c.aggregate(p.Payload) {
					t.Errorf("packet %d is an aggregation packet", i)
				}
				if want := i == len(pkts)-1; p.Marker != want {
					t.Errorf("packet %d marker %v, want %v", i, p.Marker, want)
				}
				if i > 0 && p.SequenceNumber != pkts[i-1].SequenceNumber+1 {
					t.Errorf("packet %d sequence %d after %d", i, p.SequenceNumber, pkts[i-1].SequenceNumber)
				}
			}
			for i := 0; i < 3; i++ {
				if !bytes.Equal(pkts[i].Payload, c.au[i]) {
					t.Errorf("packet %d is not NAL unit %d on its own", i, i)
				}
			}
		})
	}

	// The plain encoder does aggregate the same small slices; if it ever
	// stops, this test still holds but no longer proves anything.
	plain := &rtph265.Encoder{PayloadType: 96}
	_ = plain.Init()
	pkts, _ := plain.Encode([][]byte{{0x02, 0x01, 0x10}, {0x02, 0x01, 0x20}})
	if len(pkts) != 1 || (pkts[0].Payload[0]>>1)&0x3f != 48 {
		t.Logf("rtph265 no longer aggregates small NAL units on its own")
	}
}
