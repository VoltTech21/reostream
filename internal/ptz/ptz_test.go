package ptz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

type fakeSession struct {
	mu       sync.Mutex
	sent     []baichuan.PTZCommand
	gotos    []int
	failNext int // writes to fail before succeeding
	closed   bool
}

func (f *fakeSession) Control(_ context.Context, cmd baichuan.PTZCommand, _ int) (int16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return 0, errors.New("connection reset")
	}
	f.sent = append(f.sent, cmd)
	return 200, nil
}

func (f *fakeSession) Presets(context.Context) ([]baichuan.PTZPreset, error) {
	return []baichuan.PTZPreset{{ID: 0, Name: "home"}}, nil
}

func (f *fakeSession) GotoPreset(_ context.Context, id int) (int16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotos = append(f.gotos, id)
	return 200, nil
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSession) commands() []baichuan.PTZCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]baichuan.PTZCommand(nil), f.sent...)
}

type dialer struct {
	mu       sync.Mutex
	sessions []*fakeSession
	failures int // sessions to hand out that fail their first write
}

func (d *dialer) dial(context.Context) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &fakeSession{}
	if d.failures > 0 {
		d.failures--
		s.failNext = 1
	}
	d.sessions = append(d.sessions, s)
	return s, nil
}

func (d *dialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

func TestMoveThenStopUsesOneSession(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, time.Minute, time.Minute)
	defer c.Close()
	ctx := context.Background()

	if err := c.Move(ctx, baichuan.PTZLeft, 20); err != nil {
		t.Fatal(err)
	}
	if !c.Moving() {
		t.Error("not moving after Move")
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Moving() {
		t.Error("still moving after Stop")
	}
	if d.count() != 1 {
		t.Errorf("%d sessions, want 1", d.count())
	}
	got := d.sessions[0].commands()
	if len(got) != 2 || got[0] != baichuan.PTZLeft || got[1] != baichuan.PTZStop {
		t.Errorf("commands = %v, want [left stop]", got)
	}
}

// A stop with nothing moving and no session open must not wake the camera:
// Frigate sends one after every press, and a press may already have been
// stopped by the safety timer.
func TestStopWithNothingMovingDoesNotDial(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, time.Minute, time.Minute)
	defer c.Close()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.count() != 0 {
		t.Errorf("dialled %d times for a no-op stop", d.count())
	}
}

// A movement that is never stopped, because the client died, must not run
// into the end stop: the controller sends the stop itself.
func TestAMoveThatIsNeverStoppedIsStopped(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, time.Minute, 50*time.Millisecond)
	defer c.Close()
	if err := c.Move(context.Background(), baichuan.PTZRight, 10); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Moving() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Moving() {
		t.Fatal("safety stop never fired")
	}
	got := d.sessions[0].commands()
	if len(got) != 2 || got[1] != baichuan.PTZStop {
		t.Errorf("commands = %v, want a trailing stop", got)
	}
}

func TestTheSessionIsReleasedWhenIdle(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, 50*time.Millisecond, time.Minute)
	defer c.Close()
	ctx := context.Background()
	if err := c.Move(ctx, baichuan.PTZUp, 10); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	d.sessions[0].mu.Lock()
	closed := d.sessions[0].closed
	d.sessions[0].mu.Unlock()
	if !closed {
		t.Fatal("idle session was not closed; the camera would keep refusing others")
	}
	// And the next command opens a new one.
	if err := c.GotoPreset(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Errorf("%d sessions, want 2", d.count())
	}
}

// A session the camera dropped while idle looks fine until the first write.
func TestAStaleSessionIsRetriedOnceOnAFreshOne(t *testing.T) {
	d := &dialer{failures: 1}
	c := New(d.dial, time.Minute, time.Minute)
	defer c.Close()
	if err := c.Move(context.Background(), baichuan.PTZDown, 10); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if d.count() != 2 {
		t.Errorf("%d sessions, want 2", d.count())
	}
	if !d.sessions[0].closed {
		t.Error("the failed session was not closed")
	}
}

func TestPresetsAreListed(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, time.Minute, time.Minute)
	defer c.Close()
	ps, err := c.Presets(context.Background())
	if err != nil || len(ps) != 1 || ps[0].Name != "home" {
		t.Fatalf("presets = %v, %v", ps, err)
	}
}

func TestAClosedControllerRefuses(t *testing.T) {
	d := &dialer{}
	c := New(d.dial, time.Minute, time.Minute)
	c.Close()
	if err := c.Move(context.Background(), baichuan.PTZLeft, 1); err == nil {
		t.Fatal("Move succeeded on a closed controller")
	}
	if d.count() != 0 {
		t.Error("a closed controller dialled")
	}
}
