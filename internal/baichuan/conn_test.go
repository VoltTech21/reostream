package baichuan

import (
	"context"
	"runtime"
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
