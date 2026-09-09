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

	// startsKeyframe true: this test is about fan-out, not the sync gate.
	h.PublishKey([]byte("frame"), true)

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
			// startsKeyframe true throughout: this test is about never
			// blocking, not the sync gate.
			h.PublishKey([]byte("x"), true)
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
		// startsKeyframe true throughout: this test is about the drop path,
		// not the sync gate.
		h.PublishKey([]byte("x"), true)
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

func TestAddAudioFramesAccumulates(t *testing.T) {
	h := New(4)
	h.AddAudioFrames(1)
	h.AddAudioFrames(1)
	if got := h.Stats().AudioFrames; got != 2 {
		t.Fatalf("AudioFrames = %d, want 2", got)
	}
}

func TestThreeAudioStatesAreDistinguishable(t *testing.T) {
	// A camera with no audio at all, one whose audio is being dropped for
	// an unsupported codec, and one whose audio is flowing must all report
	// different combinations of AudioFrames and DroppedAudio: this is what
	// makes a silent camera diagnosable now that every stream declares an
	// audio track regardless of whether one ever arrives.
	silent := New(4)
	dropping := New(4)
	dropping.AddDroppedAudio(5)
	healthy := New(4)
	healthy.AddAudioFrames(5)

	if s := silent.Stats(); s.AudioFrames != 0 || s.DroppedAudio != 0 {
		t.Fatalf("silent camera: AudioFrames=%d DroppedAudio=%d, want both 0", s.AudioFrames, s.DroppedAudio)
	}
	if s := dropping.Stats(); s.AudioFrames != 0 || s.DroppedAudio == 0 {
		t.Fatalf("dropping camera: AudioFrames=%d DroppedAudio=%d, want AudioFrames 0 and DroppedAudio nonzero", s.AudioFrames, s.DroppedAudio)
	}
	if s := healthy.Stats(); s.AudioFrames == 0 || s.DroppedAudio != 0 {
		t.Fatalf("healthy camera: AudioFrames=%d DroppedAudio=%d, want AudioFrames nonzero and DroppedAudio 0", s.AudioFrames, s.DroppedAudio)
	}
}

func TestMarkDisconnectedZeroesRateNotCounters(t *testing.T) {
	h := New(4)
	h.AddAudioFrames(3)
	h.AddDroppedAudio(2)
	// Force a rate to actually land by crossing the stats window.
	h.RecordFrame(1000)
	time.Sleep(statsWindow + 10*time.Millisecond)
	h.RecordFrame(1000)
	before := h.Stats()
	if before.FPS == 0 && before.BitrateBps == 0 {
		t.Skip("rate window did not roll over in time; not what this test is about")
	}

	h.MarkDisconnected()
	after := h.Stats()
	if after.FPS != 0 || after.BitrateBps != 0 {
		t.Fatalf("after MarkDisconnected: FPS=%v BitrateBps=%v, want both 0", after.FPS, after.BitrateBps)
	}
	if after.AudioFrames != 3 || after.DroppedAudio != 2 {
		t.Fatalf("MarkDisconnected touched cumulative counters: AudioFrames=%d DroppedAudio=%d, want 3 and 2", after.AudioFrames, after.DroppedAudio)
	}
}

func TestSubscriberReceivesNothingUntilTheFirstKeyframe(t *testing.T) {
	h := New(8)
	ch, cancel := h.Subscribe()
	defer cancel()

	h.PublishKey([]byte("mid-gop-a"), false)
	h.PublishKey([]byte("mid-gop-b"), false)
	select {
	case got := <-ch:
		t.Fatalf("received %q before any keyframe", got)
	case <-time.After(50 * time.Millisecond):
	}

	h.PublishKey([]byte("KEY"), true)
	select {
	case got := <-ch:
		if string(got) != "KEY" {
			t.Fatalf("first delivery was %q, want the keyframe", got)
		}
	case <-time.After(time.Second):
		t.Fatal("keyframe was not delivered")
	}

	h.PublishKey([]byte("after"), false)
	select {
	case got := <-ch:
		if string(got) != "after" {
			t.Fatalf("second delivery was %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing delivered after the keyframe")
	}
}

func TestEachSubscriberWaitsForItsOwnKeyframe(t *testing.T) {
	// A subscriber joining after the stream is running must wait for the NEXT
	// keyframe, not inherit the fact that an earlier subscriber already saw one.
	h := New(8)
	first, cancelFirst := h.Subscribe()
	defer cancelFirst()
	h.PublishKey([]byte("KEY1"), true)
	<-first

	late, cancelLate := h.Subscribe()
	defer cancelLate()
	h.PublishKey([]byte("mid"), false)
	select {
	case got := <-late:
		t.Fatalf("late subscriber got %q before its own keyframe", got)
	case <-time.After(50 * time.Millisecond):
	}
	h.PublishKey([]byte("KEY2"), true)
	if got := <-late; string(got) != "KEY2" {
		t.Fatalf("late subscriber got %q, want KEY2", got)
	}
}

func TestPublishIsGatedBehindTheSameSyncAsPublishKey(t *testing.T) {
	// Audio flows through plain Publish, which must not reach a subscriber
	// that has not yet synced to a video keyframe: unaligned audio is noise
	// a player cannot place against the video it hasn't started decoding
	// yet. Once synced, Publish must deliver immediately, not wait for a
	// second keyframe.
	h := New(8)
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Publish([]byte("audio-before-sync"))
	select {
	case got := <-ch:
		t.Fatalf("received %q before any keyframe", got)
	case <-time.After(50 * time.Millisecond):
	}

	h.PublishKey([]byte("KEY"), true)
	if got := <-ch; string(got) != "KEY" {
		t.Fatalf("got %q, want KEY", got)
	}

	h.Publish([]byte("audio-after-sync"))
	select {
	case got := <-ch:
		if string(got) != "audio-after-sync" {
			t.Fatalf("got %q, want audio-after-sync", got)
		}
	case <-time.After(time.Second):
		t.Fatal("audio was not delivered once synced")
	}
}
