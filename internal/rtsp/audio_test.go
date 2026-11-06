package rtsp

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A camera that sends AAC must have it in the description. The description is
// fixed once the server stream is initialized, so initializing on the first
// keyframe would lock audio out permanently.
func TestAACIsOfferedWhenTheCameraSendsIt(t *testing.T) {
	frames := captureFrames(t)
	s := &Stream{}
	for _, f := range frames {
		s.Frame(f)
		if s.Ready() {
			break
		}
	}
	if !s.Ready() {
		t.Fatal("stream never became ready")
	}
	var audio int
	for _, m := range s.description().Medias {
		if m.Type == description.MediaTypeAudio {
			audio++
		}
	}
	if audio != 1 {
		t.Errorf("audio medias = %d, want 1: the fixture carries AAC", audio)
	}
}

// A camera with no audio must still become ready, and reasonably soon, rather
// than waiting forever for a track that will never arrive.
func TestVideoOnlyCameraStillBecomesReady(t *testing.T) {
	frames := captureFrames(t)
	s := &Stream{}
	sent := 0
	for _, f := range frames {
		if f.Kind == baichuan.FrameAAC || f.Kind == baichuan.FrameADPCM {
			continue
		}
		s.Frame(f)
		sent++
		if s.Ready() {
			break
		}
	}
	if !s.Ready() {
		t.Fatalf("video-only stream never became ready after %d frames", sent)
	}
	for _, m := range s.description().Medias {
		if m.Type == description.MediaTypeAudio {
			t.Error("audio offered for a camera that sent none")
		}
	}
}

// ADPCM has no RTP mapping worth carrying, and relabeling it as something
// else would hand a client bytes it cannot decode.
func TestADPCMIsNotOffered(t *testing.T) {
	frames := captureFrames(t)
	s := &Stream{}
	for _, f := range frames {
		if f.Kind == baichuan.FrameAAC {
			continue
		}
		if f.Kind == baichuan.FrameADPCM {
			f.Kind = baichuan.FrameADPCM
		}
		s.Frame(f)
		if s.Ready() {
			break
		}
	}
	if !s.Ready() {
		t.Fatal("stream never became ready")
	}
	for _, m := range s.description().Medias {
		if m.Type == description.MediaTypeAudio {
			t.Error("ADPCM was offered as an audio track")
		}
	}
}

// Audio carries no timestamp from these cameras, so it cannot be stamped from
// f.Micros, which is always zero. It is reconstructed from the video clock.
func TestAudioTimestampIsNotZero(t *testing.T) {
	frames := captureFrames(t)
	s := &Stream{}
	// Feed well past ready: the audio clock anchors to the video clock, and
	// a stream that became ready on its first audio frame has no video PTS
	// yet.
	for i, f := range frames {
		s.Frame(f)
		if s.Ready() && i > 20 {
			break
		}
	}
	var aac *baichuan.Frame
	for i := range frames {
		if frames[i].Kind == baichuan.FrameAAC {
			aac = &frames[i]
			break
		}
	}
	if aac == nil {
		t.Skip("fixture carries no AAC")
	}
	if aac.Micros != 0 {
		t.Skipf("this camera does stamp audio (%d); the reconstruction is not needed", aac.Micros)
	}
	if got, _ := s.audioRTPTime(*aac); got == 0 {
		t.Error("audio RTP timestamp is 0; it was taken from the frame instead of reconstructed")
	}
}

// A sink that states where its audio sits, on its video's clock (the
// presence stream playing a clip in), has it placed there to the tick, not
// merely within a second of the picture.
func TestStatedAudioIsPlacedWhereItSays(t *testing.T) {
	key := h265Keyframe(t)
	// AAC-LC, 16 kHz, mono: an 11 byte ADTS frame.
	adts := []byte{0xff, 0xf1, 0x60, 0x40, 0x01, 0x7f, 0xfc, 0x00, 0xc8, 0x00, 0x07}
	video := func(micros uint32) baichuan.Frame {
		return baichuan.Frame{Kind: baichuan.FrameIFrame, Codec: "H265", Data: key, Micros: micros}
	}
	s := &Stream{}
	s.Frame(video(1_000_000))
	s.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Data: adts, Micros: 1_000_000})
	if !s.Ready() {
		t.Fatal("not ready after a keyframe and an AAC frame")
	}
	s.Frame(video(1_000_000))
	s.Frame(video(1_500_000))
	at := func(micros uint32) uint32 {
		ts, _ := s.audioRTPTime(baichuan.Frame{Kind: baichuan.FrameAAC, Data: adts, Micros: micros})
		return ts
	}
	want := func(pts90 uint64) uint32 { return uint32((pts90 + s.epoch90) * 16000 / rtpClockHz) }
	// 200 ms after the first picture, which is PTS zero.
	if got := at(1_200_000); got != want(18000) {
		t.Fatalf("first stated frame at %d, want %d", got, want(18000))
	}
	if got := at(1_264_000); got != want(18000+5760) {
		t.Fatalf("next frame at %d, want %d", got, want(18000+5760))
	}
	// Five frames went by uncounted (no reader): back in place.
	if got := at(1_264_000 + 6*64_000); got != want(18000+7*5760) {
		t.Fatalf("frame after a gap at %d, want %d", got, want(18000+7*5760))
	}
}
