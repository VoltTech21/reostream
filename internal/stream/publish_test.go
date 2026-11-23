package stream

import (
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

func videoFrame(kind baichuan.FrameKind, micros uint32) baichuan.Frame {
	return baichuan.Frame{Kind: kind, Codec: "h265", Micros: micros, Data: append([]byte{0, 0, 0, 1, 0x26, 0x01}, make([]byte, 5000)...)}
}

// With no HTTP reader nothing is packed, but the stream still reports its
// bitrate; a reader that joins gets packets from the next keyframe on.
func TestPublishPacksOnlyForAReader(t *testing.T) {
	h := hub.New(64)
	mux, _ := ts.NewMuxer("h265")
	micros := uint32(0)
	next := func(kind baichuan.FrameKind) {
		publishVideo(h, mux, videoFrame(kind, micros))
		micros += 50_000
	}
	next(baichuan.FrameIFrame)
	for i := 0; i < 30; i++ {
		next(baichuan.FramePFrame)
	}
	time.Sleep(1100 * time.Millisecond)
	next(baichuan.FramePFrame)
	if st := h.Stats(); st.BitrateBps == 0 {
		t.Fatal("no bitrate reported while nobody reads")
	}

	ch, cancel := h.Subscribe()
	defer cancel()
	next(baichuan.FramePFrame)
	select {
	case <-ch:
		t.Fatal("a reader got a P frame before any keyframe")
	default:
	}
	next(baichuan.FrameIFrame)
	select {
	case b := <-ch:
		if len(b) == 0 || len(b)%ts.PacketSize != 0 {
			t.Fatalf("got %d bytes, not transport packets", len(b))
		}
	default:
		t.Fatal("a reader got nothing at the keyframe")
	}
}
