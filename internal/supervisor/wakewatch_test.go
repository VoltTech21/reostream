package supervisor

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
	"github.com/VoltTech21/reostream/internal/config"
)

// sleepFlag is a battery camera as its searches see it.
type sleepFlag struct {
	asleep   atomic.Bool
	searched atomic.Int32
	ours     atomic.Bool
}

// fastWatch shortens the wake watcher's timing, and has it search flag
// instead of the network.
func fastWatch(t *testing.T, flag *sleepFlag) {
	t.Helper()
	fastWake(t)
	every, hold, search, ours := wakeWatchEvery, wakeMotionHold, wakeSearch, wakeOurs
	wakeWatchEvery, wakeMotionHold = 5*time.Millisecond, 150*time.Millisecond
	wakeSearch = func(ctx context.Context, host string) (bcudp.Info, error) {
		flag.searched.Add(1)
		return bcudp.Info{Addr: net.ParseIP(host), Asleep: flag.asleep.Load(), Battery: -1}, nil
	}
	wakeOurs = func(net.IP, time.Duration) bool { return flag.ours.Load() }
	t.Cleanup(func() { wakeWatchEvery, wakeMotionHold, wakeSearch, wakeOurs = every, hold, search, ours })
}

// motionLog records what the motion hook was told.
type motionLog struct {
	mu  sync.Mutex
	got []bool
}

func (m *motionLog) hook(camera string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, on)
}

func (m *motionLog) calls() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.got...)
}

// The camera's PIR wakes it: the main stream wakes with nobody watching,
// motion goes on, and both end after the hold.
func TestBatteryWakingOnItsOwnIsMotion(t *testing.T) {
	var flag sleepFlag
	flag.asleep.Store(true)
	fastWatch(t, &flag)
	var wakes, awake int32
	var ml motionLog
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	s.SetMotionHook(ml.hook)
	start(t, s)

	waitFor(t, func() bool { return flag.searched.Load() >= 2 })
	flag.asleep.Store(false)
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 1 })
	// The hook can run a moment after the stream counts as awake.
	waitFor(t, func() bool { return len(ml.calls()) >= 1 })
	if got := ml.calls(); len(got) != 1 || !got[0] {
		t.Fatalf("motion hook got %v, want [true]", got)
	}
	flag.ours.Store(true) // our stream now holds it awake
	waitFor(t, func() bool { return len(ml.calls()) == 2 })
	if got := ml.calls(); got[1] {
		t.Fatalf("motion hook got %v, want [true false]", got)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 0 })
}

// A wake our own session caused (a viewer, the page, the first picture)
// is not motion.
func TestBatteryWakeOfOursIsNotMotion(t *testing.T) {
	var flag sleepFlag
	flag.asleep.Store(true)
	flag.ours.Store(true)
	fastWatch(t, &flag)
	var wakes, awake int32
	var ml motionLog
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	s.SetMotionHook(ml.hook)
	start(t, s)

	waitFor(t, func() bool { return flag.searched.Load() >= 2 })
	flag.asleep.Store(false)
	n := flag.searched.Load()
	waitFor(t, func() bool { return flag.searched.Load() >= n+5 })
	if got := ml.calls(); len(got) != 0 {
		t.Fatalf("motion hook got %v for our own wake", got)
	}
	if atomic.LoadInt32(&wakes) != 0 {
		t.Fatal("our own wake woke the stream")
	}
}

// Awake at the first search says nothing about when it woke.
func TestBatteryAwakeAtStartIsNotMotion(t *testing.T) {
	var flag sleepFlag
	fastWatch(t, &flag)
	var wakes, awake int32
	var ml motionLog
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	s.SetMotionHook(ml.hook)
	start(t, s)

	waitFor(t, func() bool { return flag.searched.Load() >= 5 })
	if got := ml.calls(); len(got) != 0 || atomic.LoadInt32(&wakes) != 0 {
		t.Fatalf("motion %v, %d wakes, for a camera that was awake all along", got, wakes)
	}
}

// An always-on camera never sleeps, so it is not searched at all.
func TestAlwaysOnBatteryIsNotWatched(t *testing.T) {
	var flag sleepFlag
	fastWatch(t, &flag)
	cam := battery
	cam.AlwaysOn = true
	var wakes, awake int32
	s := New([]config.Camera{cam}, counting(&wakes, &awake))
	start(t, s)
	waitFor(t, func() bool { return atomic.LoadInt32(&awake) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := flag.searched.Load(); n != 0 {
		t.Fatalf("searched an always-on camera %d times", n)
	}
}
