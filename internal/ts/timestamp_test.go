package ts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// captureFrames decodes internal/baichuan/testdata/h265_s2c.bin into the
// frames the camera actually sent, in the order it sent them. The
// interleaving matters here: the bug these tests exist for only appears when
// audio and video are handed to the muxer alternately, as a real stream does.
func captureFrames(t *testing.T) []baichuan.Frame {
	t.Helper()
	b, err := os.ReadFile("../baichuan/testdata/h265_s2c.bin")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	nonceReader := baichuan.NewReader(bytes.NewReader(b))
	first, err := nonceReader.Next()
	if err != nil {
		t.Fatalf("read first message: %v", err)
	}
	key := baichuan.AESKey(parseNonce(t, first.XML), "")

	r := baichuan.NewReader(bytes.NewReader(b))
	d := baichuan.NewDepacketiser()
	var frames []baichuan.Frame
	for i := 0; ; i++ {
		msg, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if i == 1 {
			r.SetAESKey(key)
		}
		if len(msg.Payload) == 0 {
			continue
		}
		d.Write(msg.Payload, msg.StartsPacket)
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			switch f.Kind {
			case baichuan.FrameIFrame, baichuan.FramePFrame, baichuan.FrameAAC:
				frames = append(frames, f)
			}
		}
	}
	return frames
}

// TestCaptureAudioCarriesNoTimestamp records the wire fact the whole fix
// rests on, so a future change to the depacketiser that starts producing
// audio timestamps is noticed here rather than being quietly assumed away.
// The audio packet header is magic(4) size(2) size(2): 8 bytes, with no
// field for a capture time, unlike the 24 and 32 byte video headers that
// carry microseconds at offset 16.
func TestCaptureAudioCarriesNoTimestamp(t *testing.T) {
	var audio, video int
	for _, f := range captureFrames(t) {
		if f.Kind == baichuan.FrameAAC {
			audio++
			if f.Micros != 0 {
				t.Fatalf("audio frame has Micros = %d, want 0: the wire carries no audio timestamp", f.Micros)
			}
			continue
		}
		video++
		if f.Micros == 0 {
			t.Fatal("video frame has Micros = 0: video is the only timestamped stream and must stay so")
		}
	}
	if audio == 0 || video == 0 {
		t.Fatalf("capture yielded %d audio and %d video frames, want both nonzero", audio, video)
	}
}

// ptsOnPID pulls the PTS out of every PES header on one PID.
func ptsOnPID(b []byte, want PID) []uint64 {
	var out []uint64
	for i := 0; i+PacketSize <= len(b); i += PacketSize {
		p := b[i : i+PacketSize]
		if p[1]&0x40 == 0 {
			continue
		}
		if PID(p[1]&0x1F)<<8|PID(p[2]) != want {
			continue
		}
		off := 4
		if p[3]&0x20 != 0 {
			off += 1 + int(p[4])
		}
		pes := p[off:]
		if len(pes) < 14 || pes[0] != 0x00 || pes[1] != 0x00 || pes[2] != 0x01 {
			continue
		}
		f := pes[9:14]
		out = append(out, uint64(f[0]>>1&0x07)<<30|
			uint64(f[1])<<22|uint64(f[2]>>1)<<15|
			uint64(f[3])<<7|uint64(f[4]>>1))
	}
	return out
}

// uptimeOffset shifts the capture's camera clock past wrapThreshold.
//
// The capture was taken at about 28 minutes of camera uptime, where the raw
// Micros values (about 1.69e9) are still below 2^31, so a zero-valued audio
// timestamp is not far enough behind them to look like a wrap and the
// original bug does not fire. A camera is normally up for far longer than
// 35m47s, which is the point where every audio frame starts reading as a
// backward step of more than half the uint32 range. Adding 2^31 here puts
// the capture in that ordinary condition without inventing any timing: the
// intervals between frames are the camera's own.
const uptimeOffset = uint32(1) << 31

// TestInterleavedAudioCausesNoFalseWrap is the regression test for the
// timestamp bug measured against a live camera on 2026-09-08: 2,265 ffmpeg
// timestamp discontinuities in 60 seconds, offsets running into the
// billions. Against the code that shared one Clock between both elementary
// streams this fails on every assertion below.
func TestInterleavedAudioCausesNoFalseWrap(t *testing.T) {
	frames := captureFrames(t)
	m, err := NewMuxerWithAudio("h265", "aac")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var videoMicros []uint32
	for _, f := range frames {
		if f.Kind != baichuan.FrameAAC {
			f.Micros += uptimeOffset
			videoMicros = append(videoMicros, f.Micros)
		}
		out.Write(m.Frame(f))
	}

	if got := m.clock.Wraps(); got != 0 {
		t.Errorf("clock counted %d wraps over a 3.5 second capture, want 0: a uint32 microsecond counter cannot wrap in under 71.6 minutes", got)
	}

	video := ptsOnPID(out.Bytes(), PIDVideo)
	if len(video) < 2 {
		t.Fatalf("only %d video PES headers found", len(video))
	}
	// The muxer drops frames before the first keyframe, so line the emitted
	// timestamps up with the tail of the raw ones.
	raw := videoMicros[len(videoMicros)-len(video):]
	for i := 1; i < len(video); i++ {
		wantDelta := uint64(raw[i]-raw[i-1]) * 9 / 100
		gotDelta := video[i] - video[i-1]
		if diffTicks(gotDelta, wantDelta) > 1 {
			t.Fatalf("video PTS step %d = %d ticks, want %d from the camera's own %d microsecond gap",
				i, gotDelta, wantDelta, raw[i]-raw[i-1])
		}
	}

	// 16 kHz AAC-LC, one 1024 sample block per frame: 64ms, 5760 ticks.
	// Asserting the exact step, not just monotonicity, is what catches audio
	// being pinned to the video frame times or drifting at the wrong rate.
	audio := ptsOnPID(out.Bytes(), PIDAudio)
	if len(audio) < 2 {
		t.Fatalf("only %d audio PES headers found", len(audio))
	}
	for i := 1; i < len(audio); i++ {
		if got := audio[i] - audio[i-1]; got != 5760 {
			t.Fatalf("audio PTS step %d = %d ticks, want 5760 (1024 samples at 16 kHz)", i, got)
		}
	}
}

// TestAudioStaysInStepWithVideo guards the property the obvious fix breaks.
// Giving each elementary stream its own Clock rebased to its own first frame
// removes the false wraps and puts both tracks at PTS 0 regardless of the
// real offset between them, which is lip sync drift nobody notices until a
// recording is reviewed weeks later.
func TestAudioStaysInStepWithVideo(t *testing.T) {
	frames := captureFrames(t)
	m, err := NewMuxerWithAudio("h265", "aac")
	if err != nil {
		t.Fatal(err)
	}

	// A tenth of a second. Audio and video arrive interleaved on one TCP
	// connection, so their presentation times can never legitimately be
	// further apart than a couple of frame intervals.
	const tolerance = 9000

	var lastVideo uint64
	var sawVideo, checked int
	for _, f := range frames {
		f.Micros += uptimeOffset
		chunk := m.Frame(f)
		if f.Kind != baichuan.FrameAAC {
			if v := ptsOnPID(chunk, PIDVideo); len(v) > 0 {
				lastVideo = v[0]
				sawVideo++
			}
			continue
		}
		a := ptsOnPID(chunk, PIDAudio)
		if len(a) == 0 || sawVideo == 0 {
			continue
		}
		if diffTicks(a[0], lastVideo) > tolerance {
			t.Fatalf("audio PTS %d is %d ticks from the video PTS %d alongside it, want within %d",
				a[0], diffTicks(a[0], lastVideo), lastVideo, tolerance)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no audio frame was checked against video")
	}
}

func TestAudioClockAnchorsToVideoRatherThanZero(t *testing.T) {
	// A muxer that has been running a while hands the audio clock a large
	// video PTS. Audio must start there, not at zero: starting at zero is
	// exactly what a per-stream clock rebased to its own first frame does,
	// and it puts the audio track hours ahead of the picture.
	a := NewAudioClock()
	const videoPTS = 90000 * 600 // ten minutes in
	adts := []byte{0xFF, 0xF1, 0x60, 0x40, 0x40, 0xFF, 0xFC}
	if got := a.PTS(videoPTS, adts); got != videoPTS {
		t.Fatalf("first audio PTS = %d, want %d, the video position it arrived alongside", got, uint64(videoPTS))
	}
	if got := a.PTS(videoPTS, adts); got != videoPTS+5760 {
		t.Fatalf("second audio PTS = %d, want %d", got, uint64(videoPTS)+5760)
	}
}

func TestAudioClockResyncsAfterALongGap(t *testing.T) {
	// Lost audio frames leave the sample count behind the picture. Counting
	// on from the old anchor would hold that gap for the life of the stream,
	// so a drift past a second re-anchors.
	a := NewAudioClock()
	adts := []byte{0xFF, 0xF1, 0x60, 0x40, 0x40, 0xFF, 0xFC}
	a.PTS(0, adts)
	got := a.PTS(90000*30, adts)
	if got < 90000*30-5760 || got > 90000*30 {
		t.Fatalf("audio PTS after a 30 second gap = %d, want it re-anchored near %d", got, 90000*30)
	}
	// Never backwards, even when the video position it re-anchors to is
	// behind where audio had already reached.
	if next := a.PTS(0, adts); next < got {
		t.Fatalf("audio PTS went backwards: %d then %d", got, next)
	}
}

func TestAudioClockReadsTheRateFromADTS(t *testing.T) {
	// sampling_frequency_index 3 is 48000, so a 1024 sample frame is 1920
	// ticks, not the 5760 of the 16 kHz streams these cameras normally send.
	a := NewAudioClock()
	adts := []byte{0xFF, 0xF1, 0x4C, 0x40, 0x40, 0xFF, 0xFC}
	a.PTS(0, adts)
	if got := a.PTS(0, adts); got != 1920 {
		t.Fatalf("48 kHz audio step = %d ticks, want 1920", got)
	}
}
