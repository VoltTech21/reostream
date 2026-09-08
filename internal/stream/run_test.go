package stream

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// h265Fixture loads the real H.265 capture internal/baichuan's own tests use
// to exercise the frame loop, so this package's fake-camera tests replay the
// same known-good bytes rather than inventing a second fixture.
func h265Fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../baichuan/testdata/h265_s2c.bin")
	if err != nil {
		t.Fatalf("read h265 fixture: %v", err)
	}
	return b
}

// pmtStreamType picks the stream_type byte out of a PAT+PMT header built by
// ts.NewMuxer's Header: two 188 byte packets, PAT then PMT, with the PMT's
// stream_type at a fixed offset because writePMT never varies the fields
// ahead of it. This is how the test checks which codec the muxer was built
// for without reaching into stream's unexported mux variable.
func pmtStreamType(t *testing.T, header []byte) byte {
	t.Helper()
	const pmtStreamTypeOffset = ts.PacketSize + 17
	if len(header) <= pmtStreamTypeOffset {
		t.Fatalf("header is %d bytes, too short to hold a PMT", len(header))
	}
	return header[pmtStreamTypeOffset]
}

func TestRunAgainstAFakeCamera(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(64)

	ch, cancelSub := h.Subscribe()
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		Name:     "test",
		Address:  cam.Addr(),
		Username: "admin",
		Password: "",
		Stream:   "main",
	}

	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, cfg, h) }()

	// A frame reaches the hub, and by the time it does the header is already
	// set: SetHeader is called synchronously in Run before the matching
	// Publish, in the same goroutine, so a subscriber can never observe a
	// published chunk before Header() is non-nil.
	select {
	case chunk := <-ch:
		if len(chunk) == 0 {
			t.Fatal("published an empty chunk")
		}
		if h.Header() == nil {
			t.Error("a chunk was published before the header was set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached the hub")
	}

	// The muxer is constructed from the first frame's codec: the fixture is
	// HEVC, so the header's PMT must declare StreamTypeHEVC.
	if got := pmtStreamType(t, h.Header()); got != ts.StreamTypeHEVC {
		t.Errorf("PMT stream_type = %#x, want %#x (HEVC)", got, ts.StreamTypeHEVC)
	}

	// The connection is closed when the context is cancelled: Run must
	// return promptly with ctx.Err(), and the fake camera's side of the
	// connection must run to completion (fakecam.Camera.serve only counts a
	// connection once the peer, this package's conn.Close, has closed it).
	cancel()
	select {
	case err := <-runDone:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	deadline := time.Now().Add(2 * time.Second)
	for cam.Closes() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if cam.Closes() == 0 {
		t.Error("the connection to the fake camera was never closed")
	}
}

func TestRunSetsHeaderBeforeStartingToPublish(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{Name: "test", Address: cam.Addr(), Stream: "main"}
	go Run(ctx, cfg, h)

	// Subscribe late, after frames are already flowing, the way a real
	// client joining mid-stream does. Whatever chunk it first receives, the
	// header must already be set: a subscriber that races Publish against
	// SetHeader and loses gets no PAT/PMT and stalls until the next table
	// repeat, which internal/server's own comment on Subscribe ordering
	// warns about.
	deadline := time.Now().Add(5 * time.Second)
	for h.Header() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.Header() == nil {
		t.Fatal("header was never set")
	}

	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	select {
	case <-ch:
		if h.Header() == nil {
			t.Error("header became unset after being set once")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached a late subscriber")
	}
}
