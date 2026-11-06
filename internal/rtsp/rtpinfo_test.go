package rtsp

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/headers"
)

// playRTPInfo runs DESCRIBE, SETUP and PLAY over TCP and returns the medias
// and the RTP-Info entries of the PLAY reply, in SETUP order. The client is
// closed when the test ends.
func playRTPInfo(t *testing.T, addr, path string) ([]*description.Media, headers.RTPInfo) {
	t.Helper()
	u, err := base.ParseURL("rtsp://" + addr + "/" + path)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	t.Cleanup(c.Close)
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := c.Play(nil)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	var ri headers.RTPInfo
	if err := ri.Unmarshal(res.Header["RTP-Info"]); err != nil {
		t.Fatalf("RTP-Info %q: %v", res.Header["RTP-Info"], err)
	}
	if len(ri) != len(desc.Medias) {
		t.Fatalf("RTP-Info has %d entries for %d medias", len(ri), len(desc.Medias))
	}
	return desc.Medias, ri
}

// checkRTPInfoAgrees fails unless every media has an RTP-Info timestamp and
// they all name the same instant. Each media's timestamp is read as an
// offset from the wall clock at PLAY, on that media's own clock, the way
// the stream maps its timeline to RTP (see audioRTPTime).
func checkRTPInfoAgrees(t *testing.T, medias []*description.Media, ri headers.RTPInfo, at time.Time) {
	t.Helper()
	ref := wallEpoch90(at)
	offsets := make([]time.Duration, len(ri))
	for i, e := range ri {
		if e.Timestamp == nil {
			t.Fatalf("RTP-Info for the %s media has no rtptime (%q): the reader has to guess where it starts",
				medias[i].Type, ri.Marshal())
		}
		rate := uint64(medias[i].Formats[0].ClockRate())
		ticks := int32(*e.Timestamp - uint32(ref*rate/rtpClockHz))
		offsets[i] = time.Duration(int64(ticks) * int64(time.Second) / int64(rate))
	}
	for i := 1; i < len(offsets); i++ {
		if d := offsets[i] - offsets[0]; d > 2*time.Millisecond || d < -2*time.Millisecond {
			t.Fatalf("RTP-Info puts the %s media %v from the %s media (%v vs %v); a reader that starts "+
				"each media there steps one of them back when the sender reports line them up",
				medias[i].Type, d, medias[0].Type, offsets[i], offsets[0])
		}
	}
}

// The first reader of a stream has to be told where every media starts.
//
// Nothing is packetized until someone reads, so when the first reader of a
// stream PLAYed, gortsplib had never seen a packet on it and sent RTP-Info
// with no rtptime. ffmpeg then starts each media's clock at that media's own
// first packet, and when each media's first sender report arrives it moves
// that media onto the report's timeline: the media whose first packet sat
// later on the timeline jumps back by the difference. On the emulated Duo 2
// (2026-11-04) that was the video repeating the timestamp of the frame
// before at its first keyframe; in the 2026-11-03 firmware sweep it was one
// "non monotonically increasing dts" on 32 of 212 RTSP captures, every one a
// first reader after a restart, and Frigate's recorder is exactly that after
// every restart of reostream.
func TestFirstReaderGetsRTPInfoForEveryMedia(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam_main")
	feedUntilReady(t, st, frames)
	// Carry on with nobody watching, as a stream does between start-up and
	// its first reader.
	feed(t, st, frames, len(frames))

	medias, ri := playRTPInfo(t, srv.Addr(), "cam_main")
	at := time.Now()
	if len(medias) < 2 {
		t.Fatalf("%d medias; the fixture carries video and AAC", len(medias))
	}
	checkRTPInfoAgrees(t, medias, ri, at)
}

// RTP-Info has to hold up when the stream's timeline is ahead of the wall
// clock, which is the usual case: the timeline starts at the wall clock of
// the first frame, and a camera that opens with a buffered burst delivers
// its first frame late, so every later frame is stamped ahead of when it
// arrived. gortsplib counted the time since the last packet as an unsigned
// number, which a negative interval wraps on amd64 but clamps to zero on
// arm64, so on an arm64 host each media's rtptime was its own last packet
// and video and audio named different instants.
func TestRTPInfoAgreesWhenTheTimelineRunsAhead(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam_main")
	ahead := 3 * time.Second
	st.now = func() time.Time { return time.Now().Add(ahead) }
	feedUntilReady(t, st, frames)

	// A first reader, so packets are written with the timeline ahead.
	playRTPInfo(t, srv.Addr(), "cam_main")
	feed(t, st, frames, len(frames))

	medias, ri := playRTPInfo(t, srv.Addr(), "cam_main")
	checkRTPInfoAgrees(t, medias, ri, time.Now())
}
