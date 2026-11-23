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
		return bcudp.Info{Addr: net.ParseIP(host), Asleep: flag.asleep.Load(), Battery: -1, RTT: 90 * time.Millisecond}, nil
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

// #48: a camera whose sensor wakes it every minute or two, and records
// nothing, must not have its main stream woken each time; that drained an
// Argus PT Ultra by half overnight. After wakeBurstMax wakes in
// wakeBurstWindow the guard pauses, and a burst right after a pause
// pauses for twice as long, up to wakePauseMax.
func TestWakeGuardPausesABurst(t *testing.T) {
	var g wakeGuard
	t0 := time.Date(2026, 11, 20, 21, 47, 43, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	// The #48 log: wakes at +0, +2m18s, +3m27s, +4m51s.
	for _, d := range []time.Duration{0, 138 * time.Second, 207 * time.Second} {
		if ok, _ := g.allow(at(d)); !ok {
			t.Fatalf("wake at +%v refused; the first %d in a window wake the stream", d, wakeBurstMax)
		}
	}
	ok, paused := g.allow(at(291 * time.Second))
	if ok || paused != wakePauseBase {
		t.Fatalf("4th wake in %v: allow=%v pause=%v, want refused with a %v pause", wakeBurstWindow, ok, paused, wakePauseBase)
	}
	// Inside the pause: refused, and no new pause announced.
	if ok, p := g.allow(at(291*time.Second + wakePauseBase - time.Second)); ok || p != 0 {
		t.Fatalf("wake inside the pause: allow=%v pause=%v", ok, p)
	}
	if g.skipped != 2 {
		t.Fatalf("skipped = %d, want 2", g.skipped)
	}

	// Straight after the pause the wakes go on: three more pass, the
	// fourth pauses for twice as long.
	end := 291*time.Second + wakePauseBase
	for i := 0; i < wakeBurstMax; i++ {
		if ok, _ := g.allow(at(end + time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("wake %d after the pause refused", i)
		}
	}
	if ok, p := g.allow(at(end + 3*time.Minute)); ok || p != 2*wakePauseBase {
		t.Fatalf("burst right after a pause: allow=%v pause=%v, want %v", ok, p, 2*wakePauseBase)
	}
	// It never pauses for longer than wakePauseMax.
	now := end + 3*time.Minute
	for i := 0; i < 6; i++ {
		now += g.pause
		for j := 0; j < wakeBurstMax; j++ {
			g.allow(at(now))
		}
		if _, p := g.allow(at(now)); p > wakePauseMax {
			t.Fatalf("paused for %v, over %v", p, wakePauseMax)
		}
	}
}

// Wakes spread out (real motion now and then) never pause, and a burst
// after a long quiet spell pauses for the base time again.
func TestWakeGuardLetsSpreadWakesThrough(t *testing.T) {
	var g wakeGuard
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		if ok, _ := g.allow(t0.Add(time.Duration(i) * 4 * time.Minute)); !ok {
			t.Fatalf("wake %d, 4 minutes after the last, refused", i)
		}
	}
	g = wakeGuard{pause: wakePauseMax, lastEnd: t0}
	t1 := t0.Add(3 * time.Hour)
	for i := 0; i < wakeBurstMax; i++ {
		g.allow(t1.Add(time.Duration(i) * time.Minute))
	}
	if _, p := g.allow(t1.Add(3 * time.Minute)); p != wakePauseBase {
		t.Fatalf("burst after three quiet hours paused %v, want %v", p, wakePauseBase)
	}
}

// motion_wake = false: the camera is not searched at all.
func TestMotionWakeOffIsNotWatched(t *testing.T) {
	var flag sleepFlag
	flag.asleep.Store(true)
	fastWatch(t, &flag)
	cam := battery
	off := false
	cam.MotionWake = &off
	var wakes, awake int32
	s := New([]config.Camera{cam}, counting(&wakes, &awake))
	start(t, s)
	time.Sleep(100 * time.Millisecond)
	if n := flag.searched.Load(); n != 0 {
		t.Fatalf("searched a motion_wake = false camera %d times", n)
	}
}
