package presence

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

type sink struct {
	mu     sync.Mutex
	frames []baichuan.Frame
}

func (s *sink) Frame(f baichuan.Frame) {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	s.mu.Unlock()
}

func (s *sink) snapshot() []baichuan.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]baichuan.Frame(nil), s.frames...)
}

func fast(t *testing.T) {
	idle, grace := idleEvery, liveGrace
	idleEvery, liveGrace = 20*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { idleEvery, liveGrace = idle, grace })
}

// video and audio split the stream's frames into its pictures and its
// sound.
func video(fs []baichuan.Frame) []baichuan.Frame {
	var out []baichuan.Frame
	for _, f := range fs {
		if f.Kind == baichuan.FrameIFrame || f.Kind == baichuan.FramePFrame {
			out = append(out, f)
		}
	}
	return out
}

func audio(fs []baichuan.Frame) []baichuan.Frame {
	var out []baichuan.Frame
	for _, f := range fs {
		if f.Kind == baichuan.FrameAAC {
			out = append(out, f)
		}
	}
	return out
}

// increasing checks the pictures' clock and the sound's each only go
// forward.
func increasing(t *testing.T, fs []baichuan.Frame) {
	t.Helper()
	for _, fs := range [][]baichuan.Frame{video(fs), audio(fs)} {
		for i := 1; i < len(fs); i++ {
			if fs[i].Micros <= fs[i-1].Micros {
				t.Fatalf("%s clock went back at frame %d: %d after %d", fs[i].Kind, i, fs[i].Micros, fs[i-1].Micros)
			}
		}
	}
}

// A seeded still gets the stream described at once, then repeats.
func TestTheStillIsPrimedThenRepeated(t *testing.T) {
	fast(t)
	s := &sink{}
	p := New("porch", s)
	p.Seed("../mp4read/testdata/h265.mp4")
	if n := len(video(s.snapshot())); n != primeFrames {
		t.Fatalf("primed with %d frames, want %d", n, primeFrames)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	fs := s.snapshot()
	if len(video(fs)) < primeFrames+3 {
		t.Fatalf("only %d frames after idling", len(video(fs)))
	}
	for _, f := range video(fs) {
		if f.Kind != baichuan.FrameIFrame || f.Codec != "H265" {
			t.Fatalf("idle frame %+v", f.Kind)
		}
	}
	increasing(t, fs)
}

// A clip that arrives is played in full, from its first keyframe, on the
// same clock; the still never interrupts it.
func TestAClipIsPlayedIntoTheStream(t *testing.T) {
	fast(t)
	s := &sink{}
	p := New("porch", s)
	p.Seed("../mp4read/testdata/h265.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	// Stop the player before fast's cleanup puts the timings back.
	t.Cleanup(func() { cancel(); <-done })
	p.Play("../mp4read/testdata/h265.mp4")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, f := range s.snapshot() {
			if f.Kind == baichuan.FramePFrame {
				n++
			}
		}
		if n == 28 { // 30 samples, 2 keyframes
			increasing(t, s.snapshot())
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("clip not played in full")
}

// Live frames pass through on the stream's clock; a frame of another codec
// (the sub stream) never reaches a stream already described as one.
func TestLiveFramesPassAndOtherCodecsDoNot(t *testing.T) {
	s := &sink{}
	p := New("porch", s)
	p.Seed("../mp4read/testdata/h265.mp4")
	base := len(video(s.snapshot()))
	p.Frame(baichuan.Frame{Kind: baichuan.FramePFrame, Codec: "H265", Data: []byte{0, 0, 0, 1, 2, 1}, Micros: 5_000_000})
	p.Frame(baichuan.Frame{Kind: baichuan.FramePFrame, Codec: "H265", Data: []byte{0, 0, 0, 1, 2, 1}, Micros: 5_066_000})
	p.Frame(baichuan.Frame{Kind: baichuan.FrameIFrame, Codec: "H264", Data: []byte{0, 0, 0, 1, 0x65}, Micros: 5_100_000})
	fs := video(s.snapshot())
	if len(fs)-base != 2 {
		t.Fatalf("%d live frames out, want the two H.265 ones", len(fs)-base)
	}
	if d := fs[len(fs)-1].Micros - fs[len(fs)-2].Micros; d != 66_000 {
		t.Fatalf("live spacing %d, want the camera's own 66000", d)
	}
}
