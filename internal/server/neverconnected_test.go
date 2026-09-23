package server

import (
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/hub"
)

// A camera at an address nothing answers reported connected, streaming,
// zero restarts and a frame age of 0, indefinitely. Age has no moment to
// measure from until something happens, so it returns 0, and 0 compares as
// healthy against any timeout.
//
// Found by pointing a fresh install at 192.0.2.50 and watching the page
// show a green "streaming" pill for a camera that had never had a TCP
// connection. The operator page's whole job is to answer "is this camera
// working", so this is the one wrong answer it must not give.
func TestAStreamThatNeverConnectedIsNotStreaming(t *testing.T) {
	var never hub.FrameStats
	if never.Seen() {
		t.Fatal("a zero FrameStats reports having seen something")
	}
	if never.Age() != 0 {
		t.Fatalf("Age = %v, want 0: this is the value that reads as healthy", never.Age())
	}

	// Both of the states that have actually happened must still read as seen,
	// or the fix would trade a false green for a false red.
	connected := hub.FrameStats{ConnectedAt: time.Now().Add(-90 * time.Second)}
	if !connected.Seen() {
		t.Error("a stream that connected but has sent no frame reports unseen")
	}
	if connected.Age() < 80*time.Second {
		t.Errorf("Age = %v, want the time since it connected", connected.Age())
	}

	delivering := hub.FrameStats{
		ConnectedAt: time.Now().Add(-90 * time.Second),
		LastFrameAt: time.Now().Add(-time.Second),
	}
	if !delivering.Seen() {
		t.Error("a stream delivering frames reports unseen")
	}
	if delivering.Age() > 5*time.Second {
		t.Errorf("Age = %v, want the time since the last frame", delivering.Age())
	}
}
