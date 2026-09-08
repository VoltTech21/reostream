package baichuan

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// A connection closed while its consumer has stopped reading must not leave
// readLoop running. Before the fix, readLoop parked on the send to c.msgs
// once the 256 slot buffer filled, and Close closing the socket only wakes a
// goroutine blocked on a *read*, so the goroutine, and the closure over its
// buffers, leaked forever.
func TestReadLoopExitsWhenClosedWhileConsumerHasStoppedReading(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))

	// Baseline after the fake camera's own permanent accept-loop goroutine
	// has started, but before Dial adds anything of its own. That isolates
	// the assertion below to what Dial and Close add and remove: the
	// per-connection readLoop, plus fakecam's per-connection serve and
	// drain goroutines, which exit on Close regardless of whether readLoop
	// also does and would otherwise mask a readLoop leak.
	runtime.GC()
	baseline := runtime.NumGoroutine()

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Never read conn.Messages(). The fixture carries thousands of wire
	// messages, far more than the 256 slot buffer, so readLoop is guaranteed
	// to fill it and park on the send well within this.
	time.Sleep(200 * time.Millisecond)
	if got := runtime.NumGoroutine(); got <= baseline {
		t.Fatalf("goroutine count is %d, want more than baseline %d before Close; readLoop may not have started", got, baseline)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Everything this test started, readLoop included, should wind down to
	// the baseline. Poll rather than sleep once, so the test is not tuned to
	// a specific scheduler delay.
	settle := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		if got := runtime.NumGoroutine(); got <= baseline {
			return
		}
		if time.Now().After(settle) {
			t.Fatalf("goroutine count settled at %d, want baseline %d; readLoop is still running", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The camera's own accounting for a stream is released by the stop message,
// not by the TCP close: a process that exits (or crashes) without sending it
// leaves the camera refusing new connections on that stream for minutes.
// This happened for real on 2026-09-08 and cost nine minutes of footage, so
// this is the highest value test in this file.
func TestCloseSendsTheStopMessage(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.StartVideo(StreamMain); err != nil {
		t.Fatalf("start video: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Wait for fakecam's side to see the connection fully closed, so
	// Received() reflects everything the client wrote before hanging up,
	// not whatever happened to have arrived by the time this goroutine got
	// scheduled.
	deadline := time.Now().Add(2 * time.Second)
	for cam.Closes() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cam.Closes() == 0 {
		t.Fatal("fake camera never observed the connection close")
	}

	// StartVideo and Close's stop message are both message id 3, sent after
	// login, so both are AES encrypted under the key this session's own
	// nonce derives. Decrypting with that key is what tells them apart from
	// each other, not from anything id-based.
	key := AESKey(conn.Nonce(), "")
	var videoXML []string
	walkMessages(cam.Received(), func(h Header, body []byte) bool {
		if h.MsgID != MsgIDVideo || len(body) == 0 {
			return true
		}
		xmlBody, err := AESDecrypt(key, body)
		if err != nil {
			t.Fatalf("decrypt a client video message: %v", err)
		}
		videoXML = append(videoXML, string(xmlBody))
		return true
	})

	if len(videoXML) < 2 {
		t.Fatalf("camera saw %d video messages from the client, want at least 2 (start, stop)", len(videoXML))
	}
	if !strings.Contains(videoXML[0], "<streamType>") {
		t.Fatalf("first video message should start a stream, got %s", videoXML[0])
	}
	if last := videoXML[len(videoXML)-1]; strings.Contains(last, "<streamType>") {
		t.Fatalf("last video message still names a stream, want the empty stop message: %s", last)
	}
}

// Conn.Ping does not schedule itself; internal/stream decides when to call
// it (elapsed-time check, never a select case beside the frame channel,
// since frames arrive roughly every 40ms and would starve a timer case
// there before it was ever chosen). This test is the other half of that
// split: that each time something decides to ping, the call actually puts a
// keepalive on the wire, framed and encrypted like any other post-login
// message, rather than merely returning nil.
func TestPingIsSentOnSchedule(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const wantPings = 3
	for i := 0; i < wantPings; i++ {
		if err := conn.Ping(); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var got int
	for {
		got = 0
		walkMessages(cam.Received(), func(h Header, body []byte) bool {
			if h.MsgID == MsgIDPing {
				got++
			}
			return true
		})
		if got >= wantPings || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != wantPings {
		t.Fatalf("camera received %d ping messages, want %d", got, wantPings)
	}
}

// A camera vanishing mid-stream must surface as an error on the read side
// promptly, not a hang: Run's consumer loop treats a closed Messages()
// channel with a non-nil Err() as the camera having gone away, and that path
// only works if readLoop actually notices and exits.
func TestMidStreamDisconnectReturnsAnError(t *testing.T) {
	// Six messages covers the login handshake and a couple of frames after
	// it, so the client observes real streaming before the camera vanishes,
	// not just a raw handshake.
	n := fixturePrefixLen(t, "h265_s2c.bin", 6)
	cam := fakecam.NewDropAfter(t, loadFixture(t, "h265_s2c.bin"), n)

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-conn.Messages():
			if !open {
				if conn.Err() == nil {
					t.Fatal("Messages closed after the camera vanished, but Err() is nil")
				}
				return
			}
			// Keep draining frames until the drop is observed.
		case <-deadline:
			t.Fatal("no error surfaced within 3s of the camera disconnecting mid-stream")
		}
	}
}

// A camera that finishes login and then sends nothing is exactly what a
// held session looks like from the client side: login succeeds, a valid
// nonce comes back, and zero frames follow. This was observed for real on
// 2026-09-08.
//
// The idle timeout is set short via Options here (see conn.go's
// DefaultIdleTimeout for the reasoning behind the production value) so this
// test resolves in milliseconds rather than waiting out a real 15s window.
func TestSilentServerDoesNotHangForever(t *testing.T) {
	loginOnly := fixturePrefixLen(t, "h265_s2c.bin", 2)
	cam := fakecam.NewPartial(t, loadFixture(t, "h265_s2c.bin"), loginOnly)

	dialDone := make(chan struct{})
	var conn *Conn
	var dialErr error
	go func() {
		conn, dialErr = Dial(context.Background(), cam.Addr(), Options{Password: "", IdleTimeout: 50 * time.Millisecond})
		close(dialDone)
	}()

	select {
	case <-dialDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Dial did not return within 2s against a camera that completes login and then goes silent")
	}
	if dialErr != nil {
		// Login itself reporting the silence (e.g. via a deadline that
		// covers more than just the handshake) would also satisfy this
		// test; only never resolving at all should not.
		return
	}
	defer conn.Close()

	select {
	case _, open := <-conn.Messages():
		if open {
			t.Fatal("received an unexpected message from a camera that never sent one")
		}
		if conn.Err() == nil {
			t.Fatal("Messages closed but Err() is nil; want a timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no idle timeout: Messages() never closed for a camera that went silent after login")
	}
}

// The idle timeout must be a gap detector, not a connection lifetime limit:
// a deadline that is set once and never refreshed would eventually kill any
// long-running stream regardless of how healthy it is, which would be a far
// worse regression than the hang this timeout exists to fix. fakecam's
// existing camera types either replay a fixture once and then stop sending
// (fakecam.New) or vanish (NewDropAfter), neither of which stays alive long
// enough to prove a *sustained* stream survives, so this test runs its own
// minimal camera that keeps writing in small, delayed chunks well past the
// configured idle window and requires that the connection survive that.
func TestHealthyStreamSurvivesPastIdleTimeout(t *testing.T) {
	const idle = 30 * time.Millisecond
	const numSteps = 40

	fixture := loadFixture(t, "h265_s2c.bin")
	loginLen := fixturePrefixLen(t, "h265_s2c.bin", 2)
	media := fixture[loginLen:]
	chunk := len(media) / numSteps

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write(fixture[:loginLen]); err != nil {
			return
		}
		// Drip media out in numSteps pieces, each well under idle apart, so
		// the whole write spans several multiples of idle. This never wraps
		// back to the start of media: doing so mid-message would splice a
		// fresh header into the middle of a body and corrupt framing, which
		// would fail this test for the wrong reason (a decode error, not an
		// idle timeout).
		for i := 0; i < numSteps; i++ {
			start := i * chunk
			end := start + chunk
			if i == numSteps-1 {
				end = len(media)
			}
			if _, err := conn.Write(media[start:end]); err != nil {
				return
			}
			time.Sleep(idle / 3)
		}
	}()

	conn, err := Dial(context.Background(), ln.Addr().String(), Options{Password: "", IdleTimeout: idle})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(10 * idle)
	got := 0
	for time.Now().Before(deadline) {
		select {
		case _, open := <-conn.Messages():
			if !open {
				t.Fatalf("connection dropped after %d messages while camera was still sending; Err(): %v", got, conn.Err())
			}
			got++
		case <-time.After(time.Until(deadline)):
		}
	}
	if got == 0 {
		t.Fatal("received no messages at all; camera goroutine may not have started")
	}
	if err := conn.Err(); err != nil {
		t.Fatalf("connection reported an error while camera was healthy: %v", err)
	}
}

// An idle timeout and a network failure both end up as a closed Messages()
// channel with a non-nil Err(), and an operator reading a log line has to be
// able to tell which one happened without cross-referencing timestamps
// against a stream's expected cadence. The error text is the only signal
// available at that point, so it must name the timeout.
func TestIdleTimeoutErrorNamesItself(t *testing.T) {
	loginOnly := fixturePrefixLen(t, "h265_s2c.bin", 2)
	cam := fakecam.NewPartial(t, loadFixture(t, "h265_s2c.bin"), loginOnly)

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: "", IdleTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-conn.Messages():
	case <-time.After(2 * time.Second):
		t.Fatal("Messages() never closed for a camera that went silent after login")
	}

	err = conn.Err()
	if err == nil {
		t.Fatal("Err() is nil after the idle timeout fired")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("Err() = %q, want it to name the idle timeout so an operator can tell it apart from a network failure", err)
	}
}

// A bad credential must be reported once, not retried: retry belongs to the
// supervisor above internal/stream, and a Dial that retries internally would
// hold the camera's one connection for that stream and block its own
// replacement from ever connecting.
func TestLoginFailureIsReportedNotRetried(t *testing.T) {
	const nonce = "doc-example-0000000000000000"
	cam := fakecam.New(t, synthesizeLoginFailureFixture(nonce))

	_, err := Dial(context.Background(), cam.Addr(), Options{Username: "admin", Password: "wrong"})
	if err == nil {
		t.Fatal("Dial succeeded against a fixture whose login reply is empty (the failure shape login() checks for)")
	}

	// Poll rather than check once: a hypothetical retry is not guaranteed to
	// have happened by the instant Dial returns its error.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cam.Accepts() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := cam.Accepts(); got != 1 {
		t.Fatalf("camera accepted %d connections, want exactly 1: Dial retried after a login failure", got)
	}
}
