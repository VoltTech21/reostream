package stream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
)

type recordingSink struct {
	mu     sync.Mutex
	frames []baichuan.Frame
}

func (r *recordingSink) Frame(f baichuan.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f)
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *recordingSink) kinds() map[baichuan.FrameKind]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[baichuan.FrameKind]int)
	for _, f := range r.frames {
		out[f.Kind]++
	}
	return out
}

// A configured sink sees the frames the muxer sees. It taps them before
// muxing, which is what lets RTSP have frames while the hub carries TS.
func TestSinkReceivesFrames(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))
	sink := &recordingSink{}
	h := hub.New(64)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = Run(ctx, Config{
		Name: "t", Address: cam.Addr(), Stream: "main", Sink: sink,
	}, h)

	if sink.count() == 0 {
		t.Fatal("sink received no frames")
	}
	if got := sink.kinds()[baichuan.FrameIFrame]; got == 0 {
		t.Error("sink saw no keyframe; RTSP cannot describe a stream without one")
	}
}

// Nil is the default and the only state the HTTP path has ever run in.
func TestNilSinkIsANoOp(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(64)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = Run(ctx, Config{Name: "t", Address: cam.Addr(), Stream: "main"}, h)

	// LastFrameAt only moves when the muxer published, so a non-zero value
	// proves the frame path ran with Sink nil rather than skipping it.
	if h.Stats().LastFrameAt.IsZero() {
		t.Fatal("no frames muxed, so the nil-sink path was not exercised")
	}
}
