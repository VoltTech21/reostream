package control

import (
	"strings"
	"sync"
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

func TestLogBufferSubscribeCancelDoesNotRaceASendToClosed(t *testing.T) {
	// A cancel deleting and closing a subscriber channel must never happen
	// while Write is sending to that same channel, or Write panics with
	// "send on closed channel". A select with a default case does not
	// protect against this: a closed channel is always ready, so the send
	// case is chosen and it panics anyway. This drives concurrent Writes
	// against a tight Subscribe/cancel loop, which is exactly the shape of
	// a browser tab opening and closing the logs page while the fleet logs.
	const iterations = 5000
	b := NewLogBuffer(10)

	var wg sync.WaitGroup
	wg.Add(2)

	panics := make(chan any, 1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				select {
				case panics <- r:
				default:
				}
			}
		}()
		for i := 0; i < iterations; i++ {
			b.Write([]byte("line\n"))
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, cancel := b.Subscribe()
			cancel()
		}
	}()

	wg.Wait()
	select {
	case r := <-panics:
		t.Fatalf("Write panicked: %v", r)
	default:
	}
}
