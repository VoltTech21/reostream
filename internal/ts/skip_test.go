package ts

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Skipping frames while nobody reads must leave the muxer exactly where
// muxing them would have: a reader joining later gets the same timestamps.
func TestSkipKeepsTheTimelineMuxingWouldHave(t *testing.T) {
	muxed, _ := NewMuxer("h265")
	skipped, _ := NewMuxer("h265")
	micros := uint32(4_000_000_000) // wraps partway through, to exercise the clock
	for i := 0; i < 200; i++ {
		kind := baichuan.FramePFrame
		if i%20 == 0 {
			kind = baichuan.FrameIFrame
		}
		f := frame(kind, micros, make([]byte, 3000)...)
		pkt := muxed.Frame(f)
		n, ok := skipped.Skip(f)
		if (pkt != nil) != ok {
			t.Fatalf("frame %d: Frame gave %d bytes, Skip ok = %v", i, len(pkt), ok)
		}
		if ok && (n < len(pkt)-PacketSize || n > len(pkt)+PacketSize) {
			t.Fatalf("frame %d: Skip sized it %d, Frame made %d", i, n, len(pkt))
		}
		micros += 50_000
	}
	f := frame(baichuan.FrameIFrame, micros, 1, 2, 3)
	a, _ := findPTS(muxed.Frame(f))
	b, ok := findPTS(skipped.Frame(f))
	if !ok || a != b {
		t.Fatalf("PTS after skipping = %d (%v), after muxing = %d", b, ok, a)
	}
}

// A muxer that has only skipped still waits for a keyframe, as Frame does.
func TestSkipWaitsForAKeyframe(t *testing.T) {
	m, _ := NewMuxer("h265")
	if _, ok := m.Skip(frame(baichuan.FramePFrame, 0, 1)); ok {
		t.Fatal("skipped a P frame before any keyframe as if it were sent")
	}
	if _, ok := m.Skip(frame(baichuan.FrameIFrame, 50_000, 1)); !ok {
		t.Fatal("keyframe not taken")
	}
}
