package stream

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/rtsp"
)

type frameSink func(baichuan.Frame)

func (f frameSink) Frame(fr baichuan.Frame) { f(fr) }

// The TrackMix zoom lens defaults to H.265 on firmware v3.0.0.5428, so the
// RTSP fallback has to take it. reostream's own RTSP server stands in for
// the camera's, serving the committed H.265 capture as Preview_02_main.
func TestNativeZoomTakesH265(t *testing.T) {
	b, err := os.ReadFile("../baichuan/testdata/h265_s2c.bin")
	if err != nil {
		t.Fatal(err)
	}
	cam := fakecam.New(t, b)
	var mu sync.Mutex
	var frames []baichuan.Frame
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = Run(ctx, Config{Name: "t", Address: cam.Addr(), Stream: "main", Sink: frameSink(func(f baichuan.Frame) {
		mu.Lock()
		defer mu.Unlock()
		f.Data = append([]byte(nil), f.Data...)
		frames = append(frames, f)
	})}, hub.New(64))
	cancel()
	if len(frames) == 0 {
		t.Fatal("no frames from the fixture")
	}

	srv := rtsp.New("127.0.0.1:18654")
	cs := srv.Add("Preview_02_main")
	if err := srv.Start(); err != nil {
		t.Skipf("rtsp server: %v", err)
	}
	t.Cleanup(srv.Close)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			for _, f := range frames {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				cs.Frame(f)
			}
		}
	}()

	old := nativeRTSPPort
	nativeRTSPPort = "18654"
	t.Cleanup(func() { nativeRTSPPort = old })

	ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer icancel()
	var codec string
	for ictx.Err() == nil {
		if codec, _, _, err = NativeInfo(ictx, "127.0.0.1", "", "", "zoom"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if codec != "H265" {
		t.Fatalf("NativeInfo: codec %q, err %v", codec, err)
	}

	var got []baichuan.Frame
	var gmu sync.Mutex
	rctx, rcancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer rcancel()
	_ = runNative(rctx, Config{Name: "z", Address: net.JoinHostPort("127.0.0.1", "9000"), Stream: "zoom", Sink: frameSink(func(f baichuan.Frame) {
		gmu.Lock()
		defer gmu.Unlock()
		got = append(got, f)
	})}, hub.New(64), "Preview_02_main")
	gmu.Lock()
	defer gmu.Unlock()
	keys := 0
	for _, f := range got {
		if f.Codec != "H265" {
			t.Fatalf("frame codec %q", f.Codec)
		}
		if f.Kind == baichuan.FrameIFrame {
			keys++
		}
	}
	if keys == 0 {
		t.Fatalf("no H.265 keyframe through the fallback (%d frames)", len(got))
	}
}
