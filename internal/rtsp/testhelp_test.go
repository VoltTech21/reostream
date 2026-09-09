package rtsp

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

// captureFrames replays the committed H265 capture through the real frame
// path and returns what came out. Tests use real camera output rather than
// hand-built NALUs, because the bugs in this area have all been about what
// cameras actually send.
func captureFrames(t *testing.T) []baichuan.Frame {
	t.Helper()
	b, err := os.ReadFile("../baichuan/testdata/h265_s2c.bin")
	if err != nil {
		t.Fatalf("read h265 fixture: %v", err)
	}
	cam := fakecam.New(t, b)

	var mu sync.Mutex
	var frames []baichuan.Frame
	sink := sinkFunc(func(f baichuan.Frame) {
		mu.Lock()
		defer mu.Unlock()
		// Copy: the frame's Data aliases a decode buffer that is reused.
		d := make([]byte, len(f.Data))
		copy(d, f.Data)
		f.Data = d
		frames = append(frames, f)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = stream.Run(ctx, stream.Config{
		Name: "t", Address: cam.Addr(), Stream: "main", Sink: sink,
	}, hub.New(64))

	mu.Lock()
	defer mu.Unlock()
	if len(frames) == 0 {
		t.Fatal("no frames captured from the fixture")
	}
	return frames
}

type sinkFunc func(baichuan.Frame)

func (f sinkFunc) Frame(fr baichuan.Frame) { f(fr) }

// h265Keyframe returns the elementary stream of the first keyframe in the
// committed capture, parameter sets included, as the camera sent it.
func h265Keyframe(t *testing.T) []byte {
	t.Helper()
	for _, f := range captureFrames(t) {
		if f.Kind == baichuan.FrameIFrame {
			return f.Video()
		}
	}
	t.Fatal("no keyframe in the fixture")
	return nil
}
