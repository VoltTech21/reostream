package baichuan_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/fakenvr"
)

func h265Capture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/h265_s2c.bin")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openSub(t *testing.T, p *baichuan.Pool, addr string, channel int, stream string) *baichuan.Sub {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, conn, err := p.Open(ctx, addr, baichuan.Options{Username: "admin"}, channel, stream)
	if err != nil {
		t.Fatalf("open channel %d %s: %v", channel, stream, err)
	}
	if conn != nil || sub == nil {
		t.Fatalf("open channel %d %s: got a connection of its own from a recorder", channel, stream)
	}
	t.Cleanup(func() { sub.Close() })
	return sub
}

// frames reads sub until it has decoded n video frames, failing on any
// message that belongs to another preview. It returns the kinds decoded,
// info packets included.
func frames(t *testing.T, sub *baichuan.Sub, channel int, stream string, n int) []baichuan.FrameKind {
	t.Helper()
	d := baichuan.NewDepacketizer()
	var kinds []baichuan.FrameKind
	video := 0
	timeout := time.After(10 * time.Second)
	for video < n {
		select {
		case m, ok := <-sub.Messages():
			if !ok {
				t.Fatalf("channel %d %s: closed after %d frames: %v", channel, stream, video, sub.Err())
			}
			if got := byte(m.Header.EncOffset); got != byte(channel) {
				t.Fatalf("channel %d %s: got a message for channel %d", channel, stream, got)
			}
			if got := byte(m.Header.EncOffset >> 8); got != baichuan.StreamID(stream) {
				t.Fatalf("channel %d %s: got a message for stream %d", channel, stream, got)
			}
			d.Write(m.Payload, m.StartsPacket)
			for {
				f, ok := d.Next()
				if !ok {
					break
				}
				kinds = append(kinds, f.Kind)
				if f.Kind == baichuan.FrameIFrame || f.Kind == baichuan.FramePFrame {
					video++
				}
			}
		case <-timeout:
			t.Fatalf("channel %d %s: only %d of %d frames", channel, stream, video, n)
		}
	}
	return kinds
}

func TestPoolRunsARecordersStreamsOnOneConnection(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	p.MaxPreviews = 8

	type want struct {
		channel int
		stream  string
	}
	streams := []want{
		{1, baichuan.StreamMain}, {1, baichuan.StreamSub}, {2, baichuan.StreamMain},
		{3, baichuan.StreamSub}, {4, baichuan.StreamExtern},
	}
	subs := make([]*baichuan.Sub, len(streams))
	for i, s := range streams {
		subs[i] = openSub(t, p, nvr.Addr(), s.channel, s.stream)
	}
	var wg sync.WaitGroup
	for i, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames(t, subs[i], s.channel, s.stream, 20)
		}()
	}
	wg.Wait()
	if n := nvr.Accepts(); n != 1 {
		t.Fatalf("recorder accepted %d connections for %d streams, want 1", n, len(streams))
	}
	if n := len(nvr.Starts()); n != len(streams) {
		t.Fatalf("recorder was asked for %d previews, want %d", n, len(streams))
	}
}

// Many streams starting at once, as they do when the daemon starts, must
// still wait for one login rather than each opening a connection.
func TestPoolStreamsStartingTogetherShareOneLogin(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	p.MaxPreviews = 8
	var wg sync.WaitGroup
	for ch := 0; ch < 8; ch++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames(t, openSub(t, p, nvr.Addr(), ch, baichuan.StreamSub), ch, baichuan.StreamSub, 5)
		}()
	}
	wg.Wait()
	if n := nvr.MaxOpen(); n != 1 {
		t.Fatalf("%d connections were open at once, want 1", n)
	}
}

func TestPoolOpensAnotherConnectionWhenOneIsFull(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	p.MaxPreviews = 2
	for ch := 0; ch < 5; ch++ {
		openSub(t, p, nvr.Addr(), ch, baichuan.StreamSub)
	}
	if n := p.Connections(); n != 3 {
		t.Fatalf("5 streams at 2 a connection used %d connections, want 3", n)
	}
	if n := nvr.Accepts(); n != 3 {
		t.Fatalf("recorder accepted %d connections, want 3", n)
	}
}

// Two readers of the same stream (a TrackMix's main and zoom behind an NVR)
// get one preview. The second, joining partway through, is handed the
// stream's picture size first and then starts on a keyframe.
func TestPoolAsksForAStreamOnceAndJoinsAtAKeyframe(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	first := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	frames(t, first, 1, baichuan.StreamMain, 3)
	second := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)

	got := frames(t, second, 1, baichuan.StreamMain, 5)
	var video []baichuan.FrameKind
	for _, k := range got {
		if k == baichuan.FrameIFrame || k == baichuan.FramePFrame {
			video = append(video, k)
		}
	}
	if got[0] != baichuan.FrameInfo {
		t.Errorf("joining reader's first packet = %v, want the info packet", got[0])
	}
	if video[0] != baichuan.FrameIFrame {
		t.Errorf("joining reader's first frame = %v, want a keyframe", video[0])
	}
	if n := len(nvr.Starts()); n != 1 {
		t.Fatalf("recorder was asked for the same stream %d times, want once", n)
	}
	// The first reader keeps going.
	frames(t, first, 1, baichuan.StreamMain, 5)
}

func TestPoolStopsAStreamWithItsLastReaderAndClosesWhenIdle(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	a := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	b := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	c := openSub(t, p, nvr.Addr(), 2, baichuan.StreamSub)

	a.Close()
	time.Sleep(50 * time.Millisecond)
	if n := len(nvr.Stops()); n != 0 {
		t.Fatalf("stream stopped with a reader still on it (%d stops)", n)
	}
	b.Close()
	waitFor(t, func() bool { return len(nvr.Stops()) == 1 }, "channel 1's stream to stop")
	frames(t, c, 2, baichuan.StreamSub, 5)
	if n := nvr.Open(); n != 1 {
		t.Fatalf("%d connections open with one stream left, want 1", n)
	}
	c.Close()
	waitFor(t, func() bool { return nvr.Open() == 0 }, "the idle connection to close")
	if n := p.Connections(); n != 0 {
		t.Fatalf("pool still holds %d connections", n)
	}
}

// A channel whose camera goes dark leaves the others streaming: its reader
// is free to give up and come back on the same connection.
func TestPoolOneChannelGoingDarkLeavesTheOthers(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	dark := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	lit := openSub(t, p, nvr.Addr(), 2, baichuan.StreamMain)
	frames(t, dark, 1, baichuan.StreamMain, 2)

	nvr.Silence(1, true)
	dark.Close() // what a stream's media watchdog does
	frames(t, lit, 2, baichuan.StreamMain, 10)
	nvr.Silence(1, false)
	again := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	frames(t, again, 1, baichuan.StreamMain, 5)
	if n := nvr.Accepts(); n != 1 {
		t.Fatalf("recorder accepted %d connections, want 1", n)
	}
}

// One reader that stops reading is dropped, rather than holding up every
// other camera on the connection.
func TestPoolDropsAStalledReader(t *testing.T) {
	defer baichuan.SetStallTimeout(100 * time.Millisecond)()
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	stalled := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	reading := openSub(t, p, nvr.Addr(), 2, baichuan.StreamMain)

	frames(t, reading, 2, baichuan.StreamMain, 600)
	waitFor(t, func() bool { return stalled.Err() != nil }, "the stalled reader to be dropped")
	for range stalled.Messages() {
	}
	if err := stalled.Err(); err != baichuan.ErrReaderStalled {
		t.Fatalf("stalled reader's error = %v", err)
	}
}

func TestPoolEndsEveryReaderWhenTheRecorderDrops(t *testing.T) {
	nvr := fakenvr.New(t, h265Capture(t))
	p := baichuan.NewPool()
	a := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	b := openSub(t, p, nvr.Addr(), 2, baichuan.StreamSub)
	frames(t, a, 1, baichuan.StreamMain, 2)
	nvr.DropAll()
	for _, s := range []*baichuan.Sub{a, b} {
		for range s.Messages() {
		}
		if s.Err() == nil {
			t.Fatal("reader ended with no error")
		}
	}
	again := openSub(t, p, nvr.Addr(), 1, baichuan.StreamMain)
	frames(t, again, 1, baichuan.StreamMain, 2)
	if n := nvr.Accepts(); n != 2 {
		t.Fatalf("recorder accepted %d connections, want 2", n)
	}
}

// A camera on its own keeps one connection per stream.
func TestPoolGivesACameraOnItsOwnItsOwnConnection(t *testing.T) {
	cam := fakecam.New(t, h265Capture(t))
	p := baichuan.NewPool()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sub, conn, err := p.Open(ctx, cam.Addr(), baichuan.Options{Username: "admin"}, 0, baichuan.StreamMain)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if sub != nil || conn == nil {
			t.Fatal("a camera on its own was given a shared connection")
		}
		conn.Close()
	}
	if n := cam.Accepts(); n != 2 {
		t.Fatalf("camera accepted %d connections, want 2", n)
	}
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
