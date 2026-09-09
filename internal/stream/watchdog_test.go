package stream

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
)

// messagePrefixLen returns the byte length of the first n messages of b,
// decoding headers with baichuan's own exported DecodeHeader. This is the
// same mechanical byte count internal/baichuan's own fixturePrefixLen test
// helper does; that one is unexported in a different package, and this is
// the only place outside internal/baichuan that needs the same thing, so it
// is duplicated here rather than exported from baichuan just for one test
// file to reach across a package boundary.
func messagePrefixLen(t *testing.T, b []byte, n int) int {
	t.Helper()
	off := 0
	for i := 0; i < n; i++ {
		h, hn, err := baichuan.DecodeHeader(b[off:])
		if err != nil {
			t.Fatalf("decode message %d: %v", i, err)
		}
		off += hn + int(h.MsgLen)
	}
	return off
}

// heldSessionKeepalive returns the bytes of a single, well-formed,
// zero-length-body message: a real ping reply or any other empty control
// message takes exactly this shape on the wire (see baichuan.Header and
// Reader.Next, which returns immediately once it sees MsgLen 0, before ever
// touching AES or the extension XML). It is what lets this test keep a
// connection answering at the socket level forever without ever handing
// stream.Run a byte of video or audio: msg.Payload is nil for a message with
// no body, so Run's `if len(msg.Payload) > 0` guard never even looks at it.
func heldSessionKeepalive() []byte {
	h := baichuan.Header{
		MsgID:   baichuan.MsgIDPing,
		Class:   baichuan.ClassModern24,
		DirByte: baichuan.DirReply,
	}
	return h.Encode()
}

// TestRunReturnsOnHeldSessionWithNoMedia reproduces the exact 2026-09-08
// production failure inside a test: a camera that completes login, then
// keeps answering on the wire (so the byte-level idle timeout never fires)
// while never sending a single video frame. This is what a camera holding a
// stale session from a dead client looks like to a new client that dials in
// and gets no frames: connected, responsive, silent.
//
// Before the media watchdog existed, Run had no way to notice this at all:
// it just sat forever in the select, servicing keepalive messages that
// carry no payload, waiting on a frame that was never coming.
func TestRunReturnsOnHeldSessionWithNoMedia(t *testing.T) {
	fixture := h265Fixture(t)
	loginOnly := messagePrefixLen(t, fixture, 2)
	prefix := fixture[:loginOnly]

	cam := fakecam.NewWriter(t, func(conn net.Conn, clientGone <-chan struct{}) {
		if _, err := conn.Write(prefix); err != nil {
			return
		}
		// A real held session answers whatever the client sends it
		// (pings included) and otherwise says nothing on its own; this
		// loop stands in for that by writing an empty message on a short
		// tick, which is enough traffic to keep a byte-level idle timeout
		// from ever firing while still being exactly zero media.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		keepalive := heldSessionKeepalive()
		for {
			select {
			case <-clientGone:
				return
			case <-ticker.C:
				if _, err := conn.Write(keepalive); err != nil {
					return
				}
			}
		}
	})

	h := hub.New(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		Name:         "held",
		Address:      cam.Addr(),
		Stream:       "main",
		MediaTimeout: 150 * time.Millisecond,
	}

	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, cfg, h) }()

	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("Run returned nil, want an error naming the held-session condition")
		}
		if !strings.Contains(err.Error(), "no video frame") {
			t.Errorf("Run error = %q, want it to name the no-media condition", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of a held session sending no media")
	}

	if got := cam.Accepts(); got != 1 {
		t.Errorf("camera accepted %d connections, want exactly 1: Run must not retry internally", got)
	}
}

// TestRunToleratesASlowButHealthyStream is the regression this feature
// exists to avoid causing: a substream-like cadence with real gaps between
// frames, including an occasional stutter, must not be torn down by the
// watchdog. Reconnecting a stream that was actually fine is worse than the
// bug this fixes, since it turns one held session into a fleet-wide
// reconnect storm on every camera that so much as hiccups.
func TestRunToleratesASlowButHealthyStream(t *testing.T) {
	fixture := h265Fixture(t)

	const (
		perMessageDelay = 8 * time.Millisecond
		// stutterAfterMsg and stutterDelay stand in for a camera hiccuping
		// under load once during the run. It lands well after the fixture's
		// own first frame (measured separately, off this same fixture: the
		// first decoded video frame takes 14 messages after login to
		// assemble, and a 4K keyframe boundary later on takes up to 10),
		// so this test is not accidentally asserting something narrower
		// than "a stutter mid-stream is tolerated": it deliberately avoids
		// compounding the stutter with the naturally largest legitimate
		// gap, which is exactly the kind of unlucky overlap that would
		// make this test flake without saying anything false about the
		// watchdog itself.
		stutterAfterMsg = 40
		stutterDelay    = 250 * time.Millisecond
		sendBudget      = 900 * time.Millisecond
		mediaTimeout    = 400 * time.Millisecond
	)

	cam := fakecam.NewWriter(t, func(conn net.Conn, clientGone <-chan struct{}) {
		off := 0
		deadline := time.Now().Add(sendBudget)
		for i := 0; off < len(fixture) && time.Now().Before(deadline); i++ {
			h, hn, err := baichuan.DecodeHeader(fixture[off:])
			if err != nil {
				return
			}
			end := off + hn + int(h.MsgLen)
			if end > len(fixture) {
				end = len(fixture)
			}
			if _, err := conn.Write(fixture[off:end]); err != nil {
				return
			}
			off = end

			delay := perMessageDelay
			if i == stutterAfterMsg {
				delay = stutterDelay
			}
			select {
			case <-clientGone:
				return
			case <-time.After(delay):
			}
		}
		// Fixture exhausted or budget spent: hold the connection open in
		// silence, same as fakecam.NewPartial, until the test tears it
		// down. Nothing further is asserted about this tail.
		<-clientGone
	})

	h := hub.New(64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		Name:         "slow",
		Address:      cam.Addr(),
		Stream:       "main",
		MediaTimeout: mediaTimeout,
	}

	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, cfg, h) }()

	// If the watchdog were wrongly tuned (or wrongly wired to fire on
	// message gaps rather than only on the absence of a decoded video
	// frame), Run would return on its own well before this deadline. Give
	// it comfortably longer than sendBudget and confirm it is still
	// running, then cancel and confirm the shutdown path (not the
	// watchdog) is what ended it.
	select {
	case err := <-runDone:
		t.Fatalf("Run returned early (%v) on a healthy, if slow and stuttering, stream", err)
	case <-time.After(sendBudget + 400*time.Millisecond):
	}

	cancel()
	select {
	case err := <-runDone:
		if err != context.Canceled {
			t.Errorf("Run returned %v after cancel, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
