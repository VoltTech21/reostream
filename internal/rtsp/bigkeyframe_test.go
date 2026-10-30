package rtsp

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A 16 MP camera's keyframe has to reach a reader whole even when the reader
// is not draining its socket while the keyframe arrives (#15). The write
// queue is drop-on-full, so a keyframe of more packets than the queue holds
// loses its tail, and Frigate records green smears until the next keyframe,
// at that resolution only.
func TestA4MBKeyframeReachesASlowReaderWhole(t *testing.T) {
	dropped, got, want := pushKeyframeToStalledReader(t, writeQueueSize, 4<<20)
	if dropped > 0 {
		t.Fatalf("%d packets of a 4 MB keyframe were dropped with a write queue of %d", dropped, writeQueueSize)
	}
	if got < want {
		t.Fatalf("the reader got %d packets of the keyframe, want %d", got, want)
	}
}

// The same push through the queue size this server had before #15. It has
// to drop, or the test above proves nothing: the kernel's socket buffers
// could be absorbing the burst on the queue's behalf.
func TestTheOldQueueDropsA4MBKeyframe(t *testing.T) {
	dropped, _, _ := pushKeyframeToStalledReader(t, 2048, 4<<20)
	if dropped == 0 {
		t.Fatal("a queue of 2048 dropped nothing, so the stalled reader is not stalling the server")
	}
}

// pushKeyframeToStalledReader writes one keyframe of about size bytes to a
// TCP reader that stops reading at the keyframe's first packet, then lets it
// read. It reports how many packets the server dropped, how many of the
// keyframe's packets the reader got in an unbroken run, and a floor on how
// many there were.
//
// Both ends' socket buffers are kept small, so the burst sits in the
// server's write queue rather than in the kernel. Left alone, loopback
// autotunes them to several megabytes, more than a LAN to a recorder gives.
func pushKeyframeToStalledReader(t *testing.T, queue, size int) (dropped uint64, got, want int) {
	t.Helper()
	frames := captureFrames(t)

	srv := New("127.0.0.1:8554")
	srv.srv.WriteQueueSize = queue
	srv.srv.Listen = func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		return smallBufferListener{ln}, nil
	}
	st := srv.Add("cam_main")
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()
	feedUntilReady(t, st, frames)

	var key, last baichuan.Frame
	for _, f := range frames {
		if f.Kind == baichuan.FrameIFrame && key.Data == nil {
			key = f
		}
		last = f
	}
	if key.Data == nil {
		t.Fatal("no keyframe in the fixture")
	}
	// The real keyframe's parameter sets and slice, then a further IDR
	// slice (H.265 type 19) of size bytes. 0xAA never forms a start code,
	// so it stays one NAL unit.
	big := key
	big.Micros = last.Micros + 100_000
	big.Data = append(append([]byte(nil), key.Data...), 0, 0, 0, 1, 19<<1, 1)
	big.Data = append(big.Data, bytes.Repeat([]byte{0xAA}, size)...)

	u, err := base.ParseURL("rtsp://" + srv.Addr() + "/cam_main")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Protocol: &tr,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(smallSocketBuffer)
			}
			return conn, err
		},
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The callback runs on the client's reader goroutine, so blocking it
	// stops the client reading its socket.
	release := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var seqs []uint16
	stalled := false
	c.OnPacketRTPAny(func(_ *description.Media, f format.Format, p *rtp.Packet) {
		if _, video := f.(*format.H265); !video {
			return
		}
		if !stalled {
			stalled = true
			<-release
		}
		mu.Lock()
		seqs = append(seqs, p.SequenceNumber)
		mu.Unlock()
		if p.Marker {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("play: %v", err)
	}

	// Frame writes every packet into the queue before it returns, and a
	// full queue is reported to OnStreamWriteError on the same goroutine,
	// so the drop count is final as soon as it returns.
	st.Frame(big)
	srv.mu.RLock()
	for _, d := range srv.drops {
		// OnStreamWriteError zeroes a count as it logs it, which the first
		// drop of each minute does, so that one is added back here.
		dropped += d.n + 1
	}
	srv.mu.RUnlock()
	close(release)

	// The payload size is gortsplib's own choice; nothing over 1,500 bytes
	// crosses an Ethernet link, so this is a floor.
	want = len(big.Data) / 1500
	if dropped > 0 {
		return dropped, 0, want
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the keyframe's last packet never arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	got = len(seqs)
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("sequence gap after %d: next is %d", seqs[i-1], seqs[i])
		}
	}
	return dropped, got, want
}

// smallSocketBuffer is small enough that the kernel holds a fraction of a
// keyframe, and large enough that a whole loopback segment (64 KB) still
// fits; below that the connection stalls outright.
const smallSocketBuffer = 128 << 10

// smallBufferListener gives the server's side of each connection a small,
// fixed send buffer, which also turns off the kernel's autotuning of it.
type smallBufferListener struct{ net.Listener }

func (l smallBufferListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(smallSocketBuffer)
	}
	return conn, err
}
