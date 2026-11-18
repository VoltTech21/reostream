package supervisor

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
	"github.com/VoltTech21/reostream/internal/config"
)

// searchLine is one search from testdata/battery-searches-2026-11-06.txt.
type searchLine struct {
	at     string // time of day the answer came, "15:10:08.774"
	rtt    time.Duration
	asleep bool
	none   bool // no answer
}

func loadSearches(t *testing.T) []searchLine {
	t.Helper()
	f, err := os.Open("testdata/battery-searches-2026-11-06.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []searchLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) != 3 {
			t.Fatalf("bad line %q", line)
		}
		l := searchLine{at: fs[0], asleep: fs[2] == "asleep", none: fs[1] == "none"}
		if !l.none {
			ms, err := strconv.Atoi(fs[1])
			if err != nil {
				t.Fatalf("bad line %q", line)
			}
			l.rtt = time.Duration(ms) * time.Millisecond
		}
		out = append(out, l)
	}
	return out
}

// span is the searches answered from from up to, not including, to.
func span(lines []searchLine, from, to string) []searchLine {
	var out []searchLine
	for _, l := range lines {
		if l.at >= from && l.at < to {
			out = append(out, l)
		}
	}
	return out
}

// replayed is what the wake watcher did with a replay.
type replayed struct {
	motion []string // the search after which motion went on, by its time
	off    int      // how many times it went off
	wakes  int32    // stream wakes
	log    string
}

// replay feeds lines to a battery camera's wake watcher as its searches,
// one per tick, with ours saying whether this process had a session with
// the camera at each, and reports what came of them.
func replay(t *testing.T, lines []searchLine, ours func(searchLine) bool) replayed {
	t.Helper()
	fastWake(t)
	var logged syncBuf
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	every, hold, search, ourFn := wakeWatchEvery, wakeMotionHold, wakeSearch, wakeOurs
	t.Cleanup(func() { wakeWatchEvery, wakeMotionHold, wakeSearch, wakeOurs = every, hold, search, ourFn })
	wakeWatchEvery, wakeMotionHold = 2*time.Millisecond, 40*time.Millisecond

	var served atomic.Int32 // searches answered so far; the last is lines[served-1]
	done := make(chan struct{})
	var once sync.Once
	wakeSearch = func(ctx context.Context, host string) (bcudp.Info, error) {
		i := int(served.Load())
		if i >= len(lines) {
			once.Do(func() { close(done) })
			<-ctx.Done()
			return bcudp.Info{}, ctx.Err()
		}
		served.Add(1)
		if lines[i].none {
			return bcudp.Info{}, errors.New("no answer")
		}
		return bcudp.Info{Addr: net.ParseIP(host), Asleep: lines[i].asleep, Battery: -1, RTT: lines[i].rtt}, nil
	}
	wakeOurs = func(net.IP, time.Duration) bool {
		i := int(served.Load()) - 1
		return ours != nil && i >= 0 && ours(lines[i])
	}

	var mu sync.Mutex
	var got replayed
	var wakes, awake int32
	s := New([]config.Camera{battery}, counting(&wakes, &awake))
	s.SetMotionHook(func(camera string, on bool) {
		mu.Lock()
		defer mu.Unlock()
		if !on {
			got.off++
			return
		}
		at := "?"
		if i := int(served.Load()) - 1; i >= 0 {
			at = lines[i].at
		}
		got.motion = append(got.motion, at)
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.Run(ctx); close(stopped) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("replay stuck at search %d of %d", served.Load(), len(lines))
	}
	cancel()
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	got.wakes = atomic.LoadInt32(&wakes)
	got.log = logged.String()
	return got
}

// asleepLeadIn is two minutes of the camera asleep with nobody connected,
// as the watcher saw it before each wake below.
func asleepLeadIn(t *testing.T) []searchLine {
	return span(loadSearches(t), "15:16:00", "15:18:00")
}

// Both PIR wakes of 11-06 are motion, from the third fast answer on: the
// battery block never left the answers, so the watcher before this saw
// neither, and Frigate got nothing either time.
func TestBatteryPIRWakeIsMotion(t *testing.T) {
	all := loadSearches(t)
	for _, c := range []struct {
		name     string
		wake     []searchLine
		wantFire string
	}{
		// 15:07:08-15:07:40, the camera already answering fast when the
		// capture began.
		{"15:07", span(all, "15:07:00", "15:07:42"), "15:07:24.775"},
		// 15:10:04-15:10:36, cloud motion at 15:10:06.257: four slow
		// answers, then fast ones, two slow ones at 204 ms, fast again.
		{"15:10", span(all, "15:10:00", "15:10:40"), "15:10:10.773"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := replay(t, append(asleepLeadIn(t), c.wake...), nil)
			if len(got.motion) != 1 || got.motion[0] != c.wantFire {
				t.Fatalf("motion after %v, want once, after the search answered at %s\n%s", got.motion, c.wantFire, got.log)
			}
			if got.off != 1 {
				t.Fatalf("motion went off %d times, want 1", got.off)
			}
			if got.wakes == 0 {
				t.Fatal("motion did not wake the stream")
			}
			if !strings.Contains(got.log, "motion: woke to record") {
				t.Fatalf("log:\n%s", got.log)
			}
		})
	}
}

// Another client connecting (11-06, 15:13:04.9: a second reostream) takes
// the battery block out of the answers until it leaves. The watcher before
// this called that motion, so every Reolink app view and every second
// reostream put a false event in Frigate; it is logged now, and nothing
// else.
func TestBatteryAnotherClientIsNotMotion(t *testing.T) {
	got := replay(t, span(loadSearches(t), "15:12:00", "15:16:30"), nil)
	if len(got.motion) != 0 || got.wakes != 0 {
		t.Fatalf("motion after %v and %d stream wakes for another client's session\n%s", got.motion, got.wakes, got.log)
	}
	if strings.Count(got.log, "another client connected") != 1 || strings.Count(got.log, "the other client left") != 1 {
		t.Fatalf("log:\n%s", got.log)
	}
}

// The same session, as our own, is neither motion nor someone else.
func TestBatteryOurSessionIsNotMotionOrAnotherClient(t *testing.T) {
	ours := func(l searchLine) bool { return l.at >= "15:13:04.9" && l.at < "15:15:59" }
	got := replay(t, span(loadSearches(t), "15:12:00", "15:16:30"), ours)
	if len(got.motion) != 0 || got.wakes != 0 || strings.Contains(got.log, "client") {
		t.Fatalf("motion after %v, %d stream wakes, for our own session\n%s", got.motion, got.wakes, got.log)
	}
}

// All 16 minutes as they came: motion once, in the 15:10 PIR wake; the
// 15:07 wake was already going when watching began, so it is not taken
// for a new one; the client at 15:13 is a log line; the 12 asleep minutes
// are nothing.
func TestBatteryWholeCaptureIsOnePIRWakeAndOneClient(t *testing.T) {
	got := replay(t, loadSearches(t), nil)
	if len(got.motion) != 1 || got.motion[0] != "15:10:10.773" {
		t.Fatalf("motion after %v, want once, after 15:10:10.773\n%s", got.motion, got.log)
	}
	if strings.Count(got.log, "another client connected") != 1 {
		t.Fatalf("log:\n%s", got.log)
	}
}
