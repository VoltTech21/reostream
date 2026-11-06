package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/stream"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &b
}

// Three streams of one camera failing every few seconds say it once, in
// words, then nothing until the reminder is due, then once when it is back.
func TestACameraFailingIsLoggedOnceThenRemindedThenRecovered(t *testing.T) {
	b := captureLog(t)
	was := failRemind
	failRemind = 50 * time.Millisecond
	t.Cleanup(func() { failRemind = was })
	s := &Supervisor{}
	auth := fmt.Errorf("stream: lounge: dial: %w (status 401)", baichuan.ErrUnauthorized)
	for i := 0; i < 9; i++ {
		s.noteFailure("lounge", auth)
	}
	if n := strings.Count(b.String(), "lounge:"); n != 1 {
		t.Fatalf("%d lines for nine failures, want 1:\n%s", n, b)
	}
	if !strings.Contains(b.String(), "wrong username or password") {
		t.Errorf("the reason is not in plain words:\n%s", b)
	}
	time.Sleep(60 * time.Millisecond)
	s.noteFailure("lounge", auth)
	if !strings.Contains(b.String(), "still failing after 10 tries") {
		t.Errorf("no reminder once it was due:\n%s", b)
	}
	s.noteRecovered("lounge")
	s.noteRecovered("lounge")
	if n := strings.Count(b.String(), "back after"); n != 1 {
		t.Errorf("%d recovery lines, want 1:\n%s", n, b)
	}
}

func TestAChangedReasonIsLogged(t *testing.T) {
	b := captureLog(t)
	s := &Supervisor{}
	s.noteFailure("cam", errors.New("dial tcp 192.0.2.1:9000: connect: no route to host"))
	s.noteFailure("cam", fmt.Errorf("x: %w", baichuan.ErrUnauthorized))
	if !strings.Contains(b.String(), "can't reach the camera") || !strings.Contains(b.String(), "now wrong username") {
		t.Errorf("want both reasons logged:\n%s", b)
	}
}
func TestANVRsTriesLeftReachesThePlainMessage(t *testing.T) {
	err := fmt.Errorf("stream: nvr: dial: %w (status 400, 8 tries left before it locks logins)", baichuan.ErrUnauthorized)
	got := plainError(err)
	if !strings.HasPrefix(got, "wrong username or password") || !strings.HasSuffix(got, "; 8 tries left before it locks logins") {
		t.Errorf("plainError = %q", got)
	}
	if got := plainError(fmt.Errorf("dial: %w", baichuan.ErrLoginRefused)); strings.Contains(got, "retrying") {
		t.Errorf("refused login says retrying itself, and the log line adds it again: %q", got)
	}
}

// A zoom stream failing while the wide lens streams on is logged under the
// stream, not the camera: "cam-19: now can't reach the camera" read as the
// whole TrackMix being down when only its zoom lens was (11-03).
func TestAZoomStreamsFailureNamesTheStream(t *testing.T) {
	b := captureLog(t)
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		if cfg.Stream == "zoom" {
			return errors.New("stream: cam-19: dial: dial tcp 192.0.2.19:9000: connect: no route to host")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "cam-19", Address: "192.0.2.19", Streams: []string{"main", "zoom"}}}, run)
	s.setBackoff(20*time.Millisecond, 50*time.Millisecond, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if !strings.Contains(b.String(), "reostream: cam-19/zoom: can't reach the camera") {
		t.Errorf("the zoom stream's failure does not name the stream:\n%s", b)
	}
	if strings.Contains(b.String(), "reostream: cam-19: ") {
		t.Errorf("a zoom failure was logged as the whole camera's:\n%s", b)
	}
}

// syncBuf is a log sink a test can read while streams are still logging.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// "back" is said once video flows, not when the camera merely takes the
// request. A TrackMix zoom stream after its two-minute wait logged "back
// after 2m" (11-03): the camera accepted the stream request, which marks
// the hub connected, and then sent nothing for 30 s and failed again.
func TestBackIsLoggedOnlyOnceVideoFlows(t *testing.T) {
	var w syncBuf
	log.SetOutput(&w)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var calls atomic.Int32
	var beforeVideo atomic.Value
	run := func(ctx context.Context, cfg stream.Config, h *hub.Hub) error {
		defer h.MarkDisconnected()
		switch calls.Add(1) {
		case 1:
			return errors.New("stream: cam-19: dial: dial tcp 192.0.2.19:9000: connect: no route to host")
		case 2:
			// Accepted, then silent: what a zoom lens with no video does.
			h.MarkConnected()
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
			}
			return fmt.Errorf("%w: no video", stream.ErrZoomUnavailable)
		}
		beforeVideo.Store(w.String())
		h.MarkConnected()
		h.RecordFrame(1000)
		<-ctx.Done()
		return ctx.Err()
	}
	s := New([]config.Camera{{Name: "cam-19", Address: "192.0.2.19", Streams: []string{"zoom"}}}, run)
	s.setBackoff(20*time.Millisecond, 50*time.Millisecond, time.Minute)
	was := retryWait
	retryWait = func(err error, backoff time.Duration) time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { retryWait = was })
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	before, _ := beforeVideo.Load().(string)
	if calls.Load() < 3 {
		t.Fatalf("only %d runs:\n%s", calls.Load(), w.String())
	}
	if strings.Contains(before, "back after") {
		t.Errorf("\"back\" was logged for a run that sent no video:\n%s", before)
	}
	if n := strings.Count(w.String(), "back after"); n != 1 {
		t.Errorf("%d \"back\" lines once video flowed, want 1:\n%s", n, w.String())
	}
}

// A stream that cannot get a camera slot fails the same way on every retry,
// but each retry is a new connection from a new local port, and the error
// text carries it: "read tcp 192.0.2.101:51234->...". Compared as text,
// every retry read as a new reason and logged a "now" line, 14 in 9 minutes
// on cam-19's zoom (11-04). The same failure from another port says nothing
// new; a different failure still does.
func TestTheSameFailureFromANewLocalPortIsNotLoggedAgain(t *testing.T) {
	b := captureLog(t)
	s := &Supervisor{}
	idle := func(port int) error {
		return fmt.Errorf("stream: cam-19: baichuan: idle timeout: no data from camera in 15s: read tcp 192.0.2.101:%d->192.0.2.19:9000: i/o timeout", port)
	}
	for _, port := range []int{51234, 51302, 51377, 51410} {
		s.noteFailure("cam-19/zoom", idle(port))
	}
	v6 := func(port int) error {
		return fmt.Errorf("stream: cam-20: read tcp [fd00::1%%eth0]:%d->[fd00::20]:9000: i/o timeout", port)
	}
	s.noteFailure("cam-20", v6(40001))
	s.noteFailure("cam-20", v6(40002))
	if n := strings.Count(b.String(), "\n"); n != 2 {
		t.Fatalf("%d lines for one repeating failure on each of two cameras, want 2:\n%s", n, b)
	}
	if !strings.Contains(b.String(), "192.0.2.101:51234->192.0.2.19:9000") {
		t.Errorf("the first line lost the addresses, which are worth seeing once:\n%s", b)
	}
	s.noteFailure("cam-19/zoom", errors.New("stream: cam-19: dial tcp 192.0.2.19:9000: connect: no route to host"))
	if !strings.Contains(b.String(), "cam-19/zoom: now can't reach the camera") {
		t.Errorf("a different failure was not logged:\n%s", b)
	}
}
