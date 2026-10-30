package rtsp

import (
	"testing"
	"time"
)

// A restart must not send RTP time backwards: each run's timeline starts
// from the wall clock, so a later run starts later than an earlier one did.
func TestTimelineStartsFromTheWallClock(t *testing.T) {
	a := wallEpoch90(time.Unix(1_700_000_000, 0))
	b := wallEpoch90(time.Unix(1_700_000_000, 500_000_000))
	if b-a != rtpClockHz/2 {
		t.Fatalf("half a second is %d ticks, want %d", b-a, rtpClockHz/2)
	}
	if now := wallEpoch90(time.Now()); now <= b {
		t.Fatal("a later run starts its timeline earlier")
	}
}

// The timeline is anchored at the first video frame, not at when the stream
// was registered. A daemon registers every stream at startup and a camera
// may take many seconds to log in and send a keyframe; anchored at
// registration, a run whose camera answered slower than the last run's
// started its timeline behind where the old one stopped, and a recorder
// kept attached through go2rtc saw time step backwards.
func TestTimelineIsAnchoredAtTheFirstFrame(t *testing.T) {
	frames := videoFrames(t)
	key := frames[0]

	wall := time.Date(2026, 10, 27, 19, 18, 0, 0, time.UTC)
	s := &Stream{now: func() time.Time { return wall }}

	// Thirty seconds pass between registration and the camera's first frame.
	wall = wall.Add(30 * time.Second)
	// With no audio in sight the stream needs audioProbeFrames frames
	// before it describes itself and starts its clock.
	key.Micros = 5_000_000
	for !s.Ready() {
		s.Frame(key)
	}

	s.mu.Lock()
	got := s.lastVideo + s.epoch90
	s.mu.Unlock()
	if want := wallEpoch90(wall); got != want {
		t.Fatalf("first frame's RTP time = %d, want %d (the wall clock at the frame, %+d ticks off)",
			got, want, int64(got)-int64(want))
	}
}

// A sender report's wall time is the RTP timeline read as time since 1970,
// so it agrees with the packets it describes.
func TestSenderReportTimeIsTheRTPTimeline(t *testing.T) {
	at := time.Date(2026, 10, 29, 14, 3, 7, 250_000_000, time.UTC)
	if got := ntpOf90(wallEpoch90(at)); !got.Equal(at) {
		t.Fatalf("ntpOf90(wallEpoch90(%v)) = %v", at, got)
	}
	if got := ntpOf90(wallEpoch90(at) + 9000); !got.Equal(at.Add(100 * time.Millisecond)) {
		t.Fatalf("9000 ticks later = %v, want 100 ms later", got)
	}
}
