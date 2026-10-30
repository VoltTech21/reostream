package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
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
