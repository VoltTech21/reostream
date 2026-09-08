package hub

import (
	"sync"
	"testing"
	"time"
)

func TestPublishReachesEverySubscriber(t *testing.T) {
	h := New(4)
	a, closeA := h.Subscribe()
	b, closeB := h.Subscribe()
	defer closeA()
	defer closeB()

	h.Publish([]byte("frame"))

	for i, ch := range []<-chan []byte{a, b} {
		select {
		case got := <-ch:
			if string(got) != "frame" {
				t.Errorf("subscriber %d got %q", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestPublishNeverBlocksOnASlowClient(t *testing.T) {
	// The whole point of the hub. A subscriber that never reads must not be
	// able to stall the camera goroutine.
	h := New(2)
	_, cancel := h.Subscribe() // deliberately never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish([]byte("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
}

func TestSlowClientIsDroppedNotBuffered(t *testing.T) {
	h := New(2)
	ch, cancel := h.Subscribe()
	defer cancel()

	for i := 0; i < 100; i++ {
		h.Publish([]byte("x"))
	}
	if h.Dropped() == 0 {
		t.Fatal("expected the stalled subscriber to have been dropped")
	}
	if h.Clients() != 0 {
		t.Fatalf("dropped subscriber still counted, Clients = %d", h.Clients())
	}
	// Its channel must be closed so the serving goroutine notices and exits
	// rather than leaking.
	drain(ch)
	if _, open := <-ch; open {
		t.Fatal("dropped subscriber's channel was left open")
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	h := New(2)
	_, cancel := h.Subscribe()
	cancel()
	cancel() // must not panic or double close
	if h.Clients() != 0 {
		t.Fatalf("Clients = %d after unsubscribe", h.Clients())
	}
}

func TestConcurrentSubscribeAndPublishIsRaceFree(t *testing.T) {
	// Run with -race. Subscribers joining and leaving while frames flow is
	// the normal condition, not an edge case.
	h := New(8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Publish([]byte("frame"))
			}
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := h.Subscribe()
				go drain(ch)
				cancel()
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func drain(ch <-chan []byte) {
	for range ch {
	}
}

func TestNewSubscriberBeforeHeaderGetsNoSpuriousWrite(t *testing.T) {
	// Header() returning nil must not turn into an empty write to the
	// subscriber. The server relies on this to decide whether to write at all.
	h := New(4)
	if h.Header() != nil {
		t.Fatalf("Header() = %v before SetHeader, want nil", h.Header())
	}
	ch, cancel := h.Subscribe()
	defer cancel()

	select {
	case got := <-ch:
		t.Fatalf("subscriber received %q before anything was published", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSetHeaderIsRaceFreeWithPublishAndSubscribe(t *testing.T) {
	// Run with -race. SetHeader is called once the muxer learns the codec,
	// which can race with frames already flowing and clients already joined.
	h := New(8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.SetHeader([]byte("HEADER"))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Publish([]byte("frame"))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			ch, cancel := h.Subscribe()
			_ = h.Header()
			go drain(ch)
			cancel()
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestRecordFrameUpdatesLastFrameAtImmediately(t *testing.T) {
	// LastFrameAt must update on every call, not only once the rate window
	// rolls over: a status page reading it right after a single frame
	// should not see a zero time and report a stream as having no age at
	// all.
	h := New(4)
	before := time.Now()
	h.RecordFrame(100)
	stats := h.Stats()
	if stats.LastFrameAt.Before(before) {
		t.Fatal("LastFrameAt was not updated by RecordFrame")
	}
	if age := stats.Age(); age < 0 || age > time.Second {
		t.Fatalf("Age() = %v, want a small positive duration", age)
	}
}

func TestAgeIsZeroBeforeAnyFrame(t *testing.T) {
	h := New(4)
	if got := h.Stats().Age(); got != 0 {
		t.Fatalf("Age() = %v before any frame, want 0", got)
	}
}

func TestAddDroppedAudioAccumulatesAcrossCalls(t *testing.T) {
	// This total must survive a muxer being rebuilt on reconnect: see the
	// comment on AddDroppedAudio for why it lives on the hub rather than
	// being read fresh off the current muxer.
	h := New(4)
	h.AddDroppedAudio(3)
	h.AddDroppedAudio(2)
	if got := h.Stats().DroppedAudio; got != 5 {
		t.Fatalf("DroppedAudio = %d, want 5", got)
	}
}
