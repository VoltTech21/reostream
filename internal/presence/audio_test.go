package presence

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakeclip"
	"github.com/VoltTech21/reostream/internal/mp4read"
	"github.com/VoltTech21/reostream/internal/ts"
)

// A clip of 50 pictures at 50 fps, its sound starting one AAC frame (64
// ms) after the first picture, so the wait for one is short.
func testClip(frag bool, sound int) fakeclip.Clip {
	return fakeclip.Clip{Fragmented: frag, FPS: 50, Frames: 50, KeyEvery: 25, AudioFrames: sound, AudioDelay: 1024}
}

// playClip plays c into a new player writing to sk, until ten stills
// (200 ms) after the clip's last picture, by which time the last of its
// sound has gone out too. A seeded player shows the clip's last keyframe
// for a while first, so the sound has run on past the still when the clip
// arrives.
func playClip(t *testing.T, sk Sink, c fakeclip.Clip, seeded bool, snapshot func() []baichuan.Frame) {
	t.Helper()
	fast(t)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, fakeclip.Build(c), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New("porch", sk)
	if seeded {
		p.Seed(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	if seeded {
		time.Sleep(300 * time.Millisecond)
	}
	p.Play(path)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		vs := video(snapshot())
		for i, f := range vs {
			if fakeclip.VideoIndex(f.Data) == c.Frames-1 && len(vs)-i > 10 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("clip not played in full")
}

// checkSound checks the stream's sound against its pictures: back to back
// frames from the first picture on, none further ahead of the picture sent
// before it than a still's interval, and every frame of the clip's sound
// once, in order, within half a frame of where it belongs. It returns how
// many of the clip's frames it found.
func checkSound(t *testing.T, fs []baichuan.Frame, c fakeclip.Clip) int {
	t.Helper()
	vs := video(fs)
	// The clip's pictures, in order, after the primed copies and stills
	// (of its first or last keyframe).
	var first int64
	if c.Frames > 0 {
		at := -1
		for i := 0; i+1 < len(vs); i++ {
			if fakeclip.VideoIndex(vs[i].Data) == 0 && fakeclip.VideoIndex(vs[i+1].Data) == 1 {
				at = i
				break
			}
		}
		if at < 0 || len(vs) < at+c.Frames {
			t.Fatal("the clip's pictures are not in the stream")
		}
		first = int64(vs[at].Micros)
		for i := 0; i < c.Frames; i++ {
			if got := fakeclip.VideoIndex(vs[at+i].Data); got != i {
				t.Fatalf("picture %d of the clip reads as %d", i, got)
			}
			// Spaced as in the clip, the stretched first one included.
			want := int64(i * fakeclip.VideoScale / c.FPS)
			if i > 0 {
				want += int64(c.Stretch)
			}
			want = want * 1_000_000 / fakeclip.VideoScale
			if d := int64(vs[at+i].Micros) - first - want; d < -1 || d > 1 {
				t.Fatalf("picture %d of the clip at %d µs in, want %d", i, int64(vs[at+i].Micros)-first, want)
			}
		}
	}
	ahead := int64(idleEvery / time.Microsecond)

	var picture int64 = -1
	var last int64 = -1
	next := 0
	for _, f := range fs {
		switch f.Kind {
		case baichuan.FrameIFrame, baichuan.FramePFrame:
			picture = int64(f.Micros)
			continue
		case baichuan.FrameAAC:
		default:
			t.Fatalf("a %s frame in the stream", f.Kind)
		}
		at := int64(f.Micros)
		if picture < 0 {
			t.Fatal("sound before any picture")
		}
		if last < 0 && at != picture {
			t.Fatalf("sound starts at %d, not with the first picture at %d", at, picture)
		}
		if last >= 0 && at-last != audioFrameMicros {
			t.Fatalf("sound frame at %d after %d: not back to back", at, last)
		}
		if at > picture+ahead {
			t.Fatalf("sound frame at %d sent %d µs ahead of the picture at %d", at, at-picture, picture)
		}
		last = at
		if bytes.Equal(f.Data, silence) {
			continue
		}
		j := fakeclip.AudioIndex(f.Data)
		if j != next {
			t.Fatalf("clip sound frame %d where %d was due", j, next)
		}
		next++
		want := first + int64(c.AudioDelay+j*1024)*1_000_000/fakeclip.AudioScale
		if d := at - want; d < -audioFrameMicros/2 || d > audioFrameMicros/2 {
			t.Fatalf("clip sound frame %d at %d, %d µs from its picture's time %d", j, at, d, want)
		}
	}
	return next
}

// A clip's sound plays in the stream beside its picture, from a plain
// file and a fragmented one alike, with silence either side of it; and
// where the first picture lasts longer than the rest, the ones after it
// keep their place beside the sound.
func TestAClipsSoundPlaysWithItsPicture(t *testing.T) {
	for _, frag := range []bool{false, true} {
		for _, seeded := range []bool{false, true} {
			c := testClip(frag, 17)
			if seeded {
				c.Stretch = 5760 // 64 ms
			}
			s := &sink{}
			playClip(t, s, c, seeded, s.snapshot)
			if n := checkSound(t, s.snapshot(), c); n != c.AudioFrames {
				t.Fatalf("fragmented %v, seeded %v: %d of the clip's %d sound frames played", frag, seeded, n, c.AudioFrames)
			}
		}
	}
}

// A clip with no sound plays as it always did, with silence beside it.
func TestAClipWithoutSoundPlaysWithSilence(t *testing.T) {
	c := testClip(true, 0)
	s := &sink{}
	playClip(t, s, c, false, s.snapshot)
	fs := s.snapshot()
	if n := checkSound(t, fs, c); n != 0 {
		t.Fatalf("%d sound frames from a clip with none", n)
	}
	if len(audio(fs)) == 0 {
		t.Fatal("no silence beside the clip")
	}
}

// The stills carry silence from the first one on, and it keeps up with
// them.
func TestStillsCarrySilence(t *testing.T) {
	fast(t)
	s := &sink{}
	p := New("porch", s)
	p.Seed("../mp4read/testdata/h265.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	fs := s.snapshot()
	if checkSound(t, fs, fakeclip.Clip{}) != 0 {
		t.Fatal("sound other than silence beside the stills")
	}
	vs, as := video(fs), audio(fs)
	end := int64(vs[len(vs)-1].Micros)
	if got := int64(as[len(as)-1].Micros); end-got >= audioFrameMicros {
		t.Fatalf("the sound stops at %d, behind the last picture at %d", got, end)
	}
}

// The sound of a clip the stream cannot carry as it is plays silent
// rather than mislabeled.
func TestSoundOfAnotherFormatIsNotPlayed(t *testing.T) {
	p := New("porch", &sink{})
	for _, a := range []mp4read.Audio{
		{Object: 2, Rate: 48000, Channels: 1, Timescale: 48000},
		{Object: 2, Rate: 16000, Channels: 2, Timescale: 16000},
		{Object: 1, Rate: 16000, Channels: 1, Timescale: 16000},
	} {
		a.Samples = []mp4read.Sample{{Size: 10, Dur: 1024}}
		if ca := p.clipAudio(&mp4read.Track{Audio: &a}, 0); ca != nil {
			t.Fatalf("%+v was taken for the stream's 16 kHz mono AAC-LC", a)
		}
	}
}

// tsSink muxes the stream's frames as the MPEG-TS output would.
type tsSink struct {
	mu  sync.Mutex
	mux *ts.Muxer
	out []byte
	fs  []baichuan.Frame
}

func (s *tsSink) Frame(f baichuan.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fs = append(s.fs, f)
	s.out = append(s.out, s.mux.Frame(f)...)
}

func (s *tsSink) snapshot() []baichuan.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]baichuan.Frame(nil), s.fs...)
}

// pes is a PES packet's start in a transport stream: its PID, its PTS
// and the first bytes of its payload.
type pes struct {
	pid  ts.PID
	pts  uint64
	head []byte
}

func readPES(t *testing.T, b []byte) []pes {
	t.Helper()
	var out []pes
	for ; len(b) >= ts.PacketSize; b = b[ts.PacketSize:] {
		p := b[:ts.PacketSize]
		if p[0] != 0x47 {
			t.Fatal("lost packet sync")
		}
		pid := ts.PID(p[1]&0x1f)<<8 | ts.PID(p[2])
		if p[1]&0x40 == 0 || (pid != ts.PIDVideo && pid != ts.PIDAudio) {
			continue
		}
		pay := p[4:]
		if p[3]&0x20 != 0 {
			pay = pay[1+int(pay[0]):]
		}
		if len(pay) < 14 || !bytes.Equal(pay[:3], []byte{0, 0, 1}) || pay[7]&0x80 == 0 {
			t.Fatalf("PID %#x: no PES header with a PTS", pid)
		}
		x := pay[9:14]
		pts := uint64(x[0]>>1&7)<<30 | uint64(x[1])<<22 | uint64(x[2]>>1)<<15 | uint64(x[3])<<7 | uint64(x[4]>>1)
		out = append(out, pes{pid, pts, pay[14:]})
	}
	return out
}

// Through the MPEG-TS muxer, the stream carries the clip's sound as AAC
// PES packets, ADTS framed, in time order with its pictures.
func TestTheStreamMuxesSoundInTimeOrder(t *testing.T) {
	m, err := ts.NewMuxerWithAudio("H264", "aac")
	if err != nil {
		t.Fatal(err)
	}
	s := &tsSink{mux: m}
	c := testClip(false, 17)
	playClip(t, s, c, true, s.snapshot)

	ps := readPES(t, s.out)
	var picture, last uint64
	var first uint64
	prev := -1
	seen, sounds := 0, 0
	for _, p := range ps {
		if p.pid == ts.PIDVideo {
			// The clip's first picture, after the stills of its last
			// keyframe.
			if i := fakeclip.VideoIndex(p.head); i == 0 && prev != 0 && first == 0 {
				first = p.pts
			} else if i >= 0 {
				prev = i
			}
			picture = p.pts
			continue
		}
		if p.head[0] != 0xff || p.head[1]&0xf0 != 0xf0 {
			t.Fatalf("audio PES %d is not ADTS: % x", sounds, p.head[:4])
		}
		if sounds > 0 && p.pts-last != 5760 {
			t.Fatalf("audio PES %d at %d after %d: not one 64 ms frame on", sounds, p.pts, last)
		}
		if p.pts > picture+uint64(idleEvery/time.Microsecond)*9/100 {
			t.Fatalf("audio PES %d at %d ahead of the picture at %d", sounds, p.pts, picture)
		}
		if j := fakeclip.AudioIndex(p.head); j >= 0 {
			want := first + uint64(c.AudioDelay+j*1024)*90_000/fakeclip.AudioScale
			if d := int64(p.pts) - int64(want); d < -2880 || d > 2880 {
				t.Fatalf("clip sound frame %d at %d, %d ticks from its picture's time %d", j, p.pts, d, want)
			}
			seen++
		}
		last = p.pts
		sounds++
	}
	if sounds != len(audio(s.snapshot())) {
		t.Fatalf("%d audio PES packets for %d sound frames", sounds, len(audio(s.snapshot())))
	}
	if seen != c.AudioFrames {
		t.Fatalf("%d of the clip's %d sound frames in the stream", seen, c.AudioFrames)
	}
}

// Between stills the sound runs on ahead of the last picture; a clip that
// arrives then starts where the sound has got to, so all of its sound
// still plays beside it.
func TestAClipStartsWhereTheSoundHasGotTo(t *testing.T) {
	// No Run here, and the stills' own interval (a second), which bounds
	// how far ahead the sound runs.
	c := testClip(true, 17)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, fakeclip.Build(c), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	p := New("porch", s)
	p.Seed(path)
	p.mu.Lock()
	still := p.clock
	p.audioLocked(p.clock + 500_000) // what Run does as half a second passes
	p.mu.Unlock()
	p.play(context.Background(), path)
	p.mu.Lock()
	p.audioLocked(p.clock + 200_000) // and the clip's last sound after it
	p.mu.Unlock()

	fs := s.snapshot()
	vs := video(fs)
	clip := vs[primeFrames]
	if int64(clip.Micros)-int64(still) < 500_000 {
		t.Fatalf("the clip starts at %d, behind the sound at %d", clip.Micros, still+500_000)
	}
	if n := checkSound(t, fs, c); n != c.AudioFrames {
		t.Fatalf("%d of the clip's %d sound frames played", n, c.AudioFrames)
	}
}
