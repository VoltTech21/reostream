package rtsp

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// feed pushes real captured frames into a stream, which is what brings it
// ready and, once a reader is attached, produces RTP.
func feed(t *testing.T, s *Stream, frames []baichuan.Frame, n int) {
	t.Helper()
	for i, f := range frames {
		if i >= n {
			return
		}
		s.Frame(f)
	}
}

// feedUntilReady runs frames through until the stream can describe itself.
// That takes more than one keyframe: the description is fixed at
// initialisation, so the stream waits to see whether audio turns up.
func feedUntilReady(t *testing.T, s *Stream, frames []baichuan.Frame) {
	t.Helper()
	for _, f := range frames {
		s.Frame(f)
		if s.Ready() {
			return
		}
	}
	t.Fatal("stream never became ready from the fixture")
}

// playAndExpectRTP drives a client through the full sequence and waits for a
// packet. Both transport tests use it, so only the transport differs.
func playAndExpectRTP(t *testing.T, c *gortsplib.Client, addr, path string, s *Stream, frames []baichuan.Frame) {
	t.Helper()

	u, err := base.ParseURL("rtsp://" + addr + "/" + path)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	c.Scheme, c.Host = u.Scheme, u.Host
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(desc.Medias) == 0 {
		t.Fatal("no medias in the description")
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := make(chan struct{}, 1)
	c.OnPacketRTPAny(func(_ *description.Media, _ format.Format, _ *rtp.Packet) {
		select {
		case got <- struct{}{}:
		default:
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("play: %v", err)
	}

	// Feed after PLAY: packetisation is gated on the reader count, so frames
	// sent before a reader attaches are deliberately dropped.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			feed(t, s, frames, len(frames))
			time.Sleep(20 * time.Millisecond)
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no RTP packet reached the client")
	}
}

func serverWithStream(t *testing.T, path string) (*Server, *Stream) {
	t.Helper()
	srv := New("127.0.0.1:8554")
	st := srv.Add(path)
	st.srv = srv.srv
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv, st
}

// A real client completing DESCRIBE, SETUP and PLAY over TCP interleaved,
// which is the default transport and the one almost every client negotiates.
// Compatibility is the entire reason this package exists, so it is tested
// against a real client rather than asserted.
func TestClientCanPlayOverTCP(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam_main")

	// Bring the stream ready so DESCRIBE has parameter sets to offer.
	feedUntilReady(t, st, frames)

	tr := gortsplib.ProtocolTCP
	playAndExpectRTP(t, &gortsplib.Client{Protocol: &tr}, srv.Addr(), "cam_main", st, frames)
}

// A stream that exists but has not seen a keyframe must not be describable.
// An SDP with no parameter sets is cached by clients and then fails to
// decode, which is worse than a 404.
func TestDescribeBeforeKeyframeIsNotFound(t *testing.T) {
	srv, _ := serverWithStream(t, "cam_main")

	u, err := base.ParseURL("rtsp://" + srv.Addr() + "/cam_main")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()

	if _, _, err := c.Describe(u); err == nil {
		t.Fatal("describe succeeded on a stream with no keyframe")
	}
}

// UDP is offered for clients that will not do anything else. It is not the
// preferred transport, but claiming support without testing it is how a
// compatibility promise quietly becomes false.
func TestClientCanPlayOverUDP(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam_main")

	feedUntilReady(t, st, frames)

	tr := gortsplib.ProtocolUDP
	playAndExpectRTP(t, &gortsplib.Client{Protocol: &tr}, srv.Addr(), "cam_main", st, frames)
}
