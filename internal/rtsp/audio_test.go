package rtsp

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A camera that sends AAC must have it in the description. The description is
// fixed once the server stream is initialised, so initialising on the first
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

// ADPCM has no RTP mapping worth carrying, and relabelling it as something
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
	if got := s.audioRTPTime(*aac); got == 0 {
		t.Error("audio RTP timestamp is 0; it was taken from the frame instead of reconstructed")
	}
}
