package control

import (
	"strings"
	"testing"
	"time"
)

func TestLogBufferKeepsTheLastNLines(t *testing.T) {
	b := NewLogBuffer(3)
	for _, s := range []string{"one\n", "two\n", "three\n", "four\n"} {
		if _, err := b.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	got := b.Lines()
	want := []string{"two", "three", "four"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLogBufferSplitsAMultiLineWrite(t *testing.T) {
	b := NewLogBuffer(10)
	b.Write([]byte("a\nb\n"))
	if len(b.Lines()) != 2 {
		t.Fatalf("got %v, want two lines", b.Lines())
	}
}

func TestLogBufferDeliversToSubscribers(t *testing.T) {
	b := NewLogBuffer(10)
	ch, cancel := b.Subscribe()
	defer cancel()
	b.Write([]byte("hello\n"))
	select {
	case line := <-ch:
		if line != "hello" {
			t.Fatalf("got %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestLogBufferNeverBlocksOnASlowSubscriber(t *testing.T) {
	// A wedged browser tab must not be able to stall the logger, which
	// every stream goroutine writes to.
	b := NewLogBuffer(10)
	_, cancel := b.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Write([]byte("line\n"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a subscriber that is not reading")
	}
}
