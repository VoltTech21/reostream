package stream

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
)

// retagVideo rewrites the channel byte (the low byte of the encryption
// offset, which a camera echoes from the request) on every video message of
// a capture, so one fixture can stand in for either of the two streams a
// zoom connection carries.
func retagVideo(t *testing.T, fixture []byte, channel byte) []byte {
	t.Helper()
	b := append([]byte(nil), fixture...)
	for off := 0; off+20 <= len(b); {
		h, n, err := baichuan.DecodeHeader(b[off:])
		if err != nil {
			break
		}
		if h.MsgID == baichuan.MsgIDVideo {
			b[off+12] = channel
		}
		off += n + int(h.MsgLen)
	}
	return b
}

// requestedVideo lists the channel and stream bytes of every video request a
// client sent, read from the plaintext headers.
func requestedVideo(t *testing.T, sent []byte) [][2]byte {
	t.Helper()
	var out [][2]byte
	for off := 0; off+20 <= len(sent); {
		h, n, err := baichuan.DecodeHeader(sent[off:])
		if err != nil {
			break
		}
		if h.MsgID == baichuan.MsgIDVideo {
			out = append(out, [2]byte{byte(h.EncOffset), byte(h.EncOffset >> 8)})
		}
		off += n + int(h.MsgLen)
	}
	return out
}

func TestZoomPublishesTheZoomLensOverBaichuan(t *testing.T) {
	cam := fakecam.New(t, retagVideo(t, h265Fixture(t), zoomChannel))
	h := hub.New(64)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, Config{Name: "zoom-on-baichuan", Address: cam.Addr(), Stream: "zoom"}, h)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no zoom frame reached the hub")
	}

	// Channel 0's sub stream first, the one that unlocks the zoom, then
	// the zoom itself on channel 1's main.
	sid := func(s string) byte { return baichuan.StreamID(s) }
	// The fake camera starts replaying video the moment the first request
	// lands, so the first frame can arrive before the second request is
	// recorded: wait for both.
	got := requestedVideo(t, cam.Received())
	for deadline := time.Now().Add(2 * time.Second); len(got) < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		got = requestedVideo(t, cam.Received())
	}
	want := [][2]byte{{0, sid(baichuan.StreamSub)}, {zoomChannel, sid(baichuan.StreamMain)}}
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("video requests = %v, want %v first", got, want)
	}
}

func TestZoomDropsTheStreamThatUnlocksIt(t *testing.T) {
	cam := fakecam.New(t, retagVideo(t, h265Fixture(t), 0))
	h := hub.New(64)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, Config{Name: "zoom-drops-channel-0", Address: cam.Addr(), Stream: "zoom"}, h)

	select {
	case <-ch:
		t.Fatal("channel 0's frames were published as the zoom")
	case <-time.After(1500 * time.Millisecond):
	}
}

// The zoom lens's sub stream (896x512 on a TrackMix, checked 2026-10-26) is
// asked for the same way as its main: channel 0's sub first, then channel
// 1's sub.
func TestZoomSubAsksForTheZoomLensSubStream(t *testing.T) {
	cam := fakecam.New(t, retagVideo(t, h265Fixture(t), zoomChannel))
	h := hub.New(64)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, Config{Name: "zoom-sub-on-baichuan", Address: cam.Addr(), Stream: "zoom_sub"}, h)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no zoom sub frame reached the hub")
	}

	sid := func(s string) byte { return baichuan.StreamID(s) }
	// The fake camera starts replaying video the moment the first request
	// lands, so the first frame can arrive before the second request is
	// recorded: wait for both.
	got := requestedVideo(t, cam.Received())
	for deadline := time.Now().Add(2 * time.Second); len(got) < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		got = requestedVideo(t, cam.Received())
	}
	want := [][2]byte{{0, sid(baichuan.StreamSub)}, {zoomChannel, sid(baichuan.StreamSub)}}
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("video requests = %v, want %v first", got, want)
	}
}

func TestZoomSubDropsTheStreamThatUnlocksIt(t *testing.T) {
	cam := fakecam.New(t, retagVideo(t, h265Fixture(t), 0))
	h := hub.New(64)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, Config{Name: "zoom-sub-drops-channel-0", Address: cam.Addr(), Stream: "zoom_sub"}, h)

	select {
	case <-ch:
		t.Fatal("channel 0's frames were published as the zoom sub")
	case <-time.After(1500 * time.Millisecond):
	}
}

// The RTSP fallback is per stream: zoom_sub having gone to RTSP must not
// move zoom there too (2026-11-02, a TrackFlex user's zoom_sub).
func TestZoomRTSPFallbackIsPerStream(t *testing.T) {
	useZoomOverRTSP("per-stream-cam", "zoom_sub")
	t.Cleanup(func() { dropZoomOverRTSP("per-stream-cam", "zoom_sub") })
	if zoomOverRTSP("per-stream-cam", "zoom") {
		t.Fatal("zoom_sub's RTSP mark moved zoom to RTSP as well")
	}
	if !zoomOverRTSP("per-stream-cam", "zoom_sub") {
		t.Fatal("zoom_sub's RTSP mark was not kept")
	}

	// And zoom still runs over Baichuan.
	cam := fakecam.New(t, retagVideo(t, h265Fixture(t), zoomChannel))
	h := hub.New(64)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{Name: "per-stream-cam", Address: cam.Addr(), Stream: "zoom"}, h)
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no zoom frame reached the hub")
	}
}

// A zoom stream on RTSP whose RTSP fails (a camera with RTSP off) goes back
// to Baichuan on the next attempt instead of staying on RTSP for good.
func TestZoomRTSPFailureGoesBackToBaichuan(t *testing.T) {
	old := nativeRTSPPort
	nativeRTSPPort = "1" // nothing listens: the RTSP attempt fails at once
	t.Cleanup(func() { nativeRTSPPort = old })

	useZoomOverRTSP("rtsp-off-cam", "zoom_sub")
	t.Cleanup(func() { dropZoomOverRTSP("rtsp-off-cam", "zoom_sub") })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, Config{Name: "rtsp-off-cam", Address: "127.0.0.1:9", Stream: "zoom_sub"}, hub.New(4))
	if !errors.Is(err, ErrZoomUnavailable) {
		t.Fatalf("Run error = %v, want ErrZoomUnavailable so the supervisor waits longer", err)
	}
	if zoomOverRTSP("rtsp-off-cam", "zoom_sub") {
		t.Fatal("zoom_sub stayed on RTSP after its RTSP attempt failed")
	}
}

// A zoom that sends nothing while channel 0's sub stream, the one that
// unlocks it, keeps flowing still times out and is marked for RTSP. The
// unlock stream used to skip the no-video check along with its media, so on
// a TrackMix whose main stream slots were taken the zoom sat at 0 fps for
// 12 minutes (cam-19, 2026-11-03).
func TestZoomTimesOutWhileTheUnlockStreamFlows(t *testing.T) {
	fixture := retagVideo(t, h265Fixture(t), 0)
	loginOnly := messagePrefixLen(t, fixture, 2)
	// Only whole video messages are repeated, as a camera streaming the
	// unlock stream sends: anything else would reach the no-video check by
	// another way, and a capture can end partway through a message, which
	// repeated would misframe the next pass.
	var media []byte
	for off := loginOnly; off+20 <= len(fixture); {
		h, n, err := baichuan.DecodeHeader(fixture[off:])
		end := off + n + int(h.MsgLen)
		if err != nil || end > len(fixture) {
			break
		}
		if h.MsgID == baichuan.MsgIDVideo {
			media = append(media, fixture[off:end]...)
		}
		off = end
	}
	cam := fakecam.NewWriter(t, func(conn net.Conn, clientGone <-chan struct{}) {
		if _, err := conn.Write(fixture[:loginOnly]); err != nil {
			return
		}
		for {
			select {
			case <-clientGone:
				return
			default:
			}
			if _, err := conn.Write(media); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	t.Cleanup(func() { dropZoomOverRTSP("unlock-only-cam", "zoom") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Name: "unlock-only-cam", Address: cam.Addr(), Stream: "zoom", MediaTimeout: 300 * time.Millisecond}
	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, cfg, hub.New(8)) }()

	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "no video frame") {
			t.Fatalf("Run error = %v, want the no-video condition", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("zoom kept waiting while only the unlock stream flowed")
	}
	if !zoomOverRTSP("unlock-only-cam", "zoom") {
		t.Fatal("zoom was not marked to try the camera's RTSP next")
	}
}
