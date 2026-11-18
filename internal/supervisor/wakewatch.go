package supervisor

import (
	"context"
	"log"
	"time"

	"github.com/VoltTech21/reostream/internal/bcudp"
)

// Live motion for a battery camera, with nothing to set up and without
// waking it: its PIR wakes the camera to record, and a search, which the
// camera answers without waking, shows that, though not the way it looks.
//
// Until 2026-11-06 this read a search that came back without its battery
// block ("asleep") as motion. That was wrong: a capture that day showed
// two PIR recordings (15:07 and 15:10) with every answer still carrying
// the block; it goes only while a client is connected. So the old watcher
// reported another client (the Reolink app, a second reostream) as motion
// and missed every real one.
//
// What a PIR wake does change is how fast the camera answers. Asleep, its
// Wi-Fi is in power save and answers at the next beacon it listens for:
// 18 to 656 ms, median 97 ms, over 736 searches. For the whole of both
// recordings it answered in 3 to 11 ms (43 searches), apart from three
// answers at about 204 ms. A camera with a client connected answers in 12
// to 77 ms, but without the block, so it is never taken for a PIR wake.
var (
	// wakeWatchEvery is how often each battery camera is searched.
	wakeWatchEvery = time.Second
	// wakeWatchTimeout bounds one search.
	wakeWatchTimeout = 900 * time.Millisecond
	// wakeOursGrace is how long after our own session a wake is still put
	// down to it.
	wakeOursGrace = 5 * time.Second
	// wakeMotionHold is how long one wake counts as motion. The camera
	// recorded for 32 s on each PIR wake on 11-06, and once our stream is
	// up it stays awake for us, so no later search can say when the motion
	// ended; the next motion after that is the next wake.
	wakeMotionHold = 30 * time.Second

	wakeSearch = bcudp.Search
	wakeOurs   = bcudp.Ours
)

const (
	// pirFast is the round trip under which an answer that still carries
	// the battery block comes from a camera awake to record. 11-06: PIR
	// wakes 3 to 11 ms, asleep never under 18 ms. It assumes reostream is
	// on the camera's LAN, as on 11-06, where the path itself adds about a
	// millisecond; a path slower than a few ms hides PIR wakes (they are
	// missed, never invented), and pirSlowPath says so in the log.
	pirFast = 15 * time.Millisecond
	// pirFastRun is how many fast answers in a row make a PIR wake. One
	// can come from a client's connect, which wakes the camera 0.9 s
	// before the battery block goes (11-06, 15:13:04.9), and none of the
	// 736 asleep answers on 11-06 was fast at all; three in a row cost
	// one second more than two and leave room for what the one day did
	// not show. On 11-06 the third came 4.5 s after the camera's own
	// cloud motion report and 6.7 s after the recording began; the camera
	// was still answering slowly for the first 4.6 s of it.
	pirFastRun = 3
	// pirArmRun is how many slow, asleep answers in a row arm the watcher:
	// at start, after each motion, and after any session (ours or someone
	// else's). So a wake already going when the watching starts is not
	// taken for a new one, one PIR wake is one motion even though it has
	// slow answers in it (11-06: two at 204 ms, 5 s in), and the camera
	// settling after a session is not motion. On 11-06 the shortest gap
	// between two wakes was over two minutes of slow answers.
	pirArmRun = 5
	// pirSlowPathAfter and pirSlowPath: when no answer in this many
	// searches came back faster than pirSlowPath, the path to the camera
	// alone is too slow for a PIR wake to come in under pirFast (asleep
	// answers bottom out about 18 ms above the path's own delay, PIR
	// answers 3 to 11 ms above it), so that is logged once.
	pirSlowPathAfter = 600
	pirSlowPath      = 30 * time.Millisecond
)

// wakeVerdict is what one search says about a battery camera.
type wakeVerdict int

const (
	wakeNothing     wakeVerdict = iota
	wakeMotion                  // its PIR woke it to record
	wakeOtherClient             // someone else (not this process) connected to it
	wakeOtherGone               // that client left and it is asleep again
	wakeSlowPath                // its answers are too slow for PIR wakes to show
)

// pirWatch reads a battery camera's searches, one at a time, as they come.
type pirWatch struct {
	fast, slow int           // fast (and asleep) and slow (and asleep) answers in a row
	armed      bool          // pirArmRun slow answers since the last motion or session
	asleep     bool          // the last answer carried the battery block
	known      bool          // there was a last answer
	other      bool          // someone else's session is what is keeping it awake
	otherSince time.Time     // when that was seen
	searches   int           // answered searches, up to pirSlowPathAfter
	fastest    time.Duration // the fastest of those
	recent     []time.Duration
}

// see takes one search's answer (or its error) and whether this process
// has a session with the camera, and says what it means.
func (w *pirWatch) see(now time.Time, info bcudp.Info, err error, ours bool) wakeVerdict {
	if err != nil {
		// Out of reach for a moment: what it was stands, but a run of
		// fast answers is broken.
		w.fast = 0
		return wakeNothing
	}
	var slowPath bool
	if w.searches < pirSlowPathAfter {
		if w.searches == 0 || info.RTT < w.fastest {
			w.fastest = info.RTT
		}
		w.searches++
		slowPath = w.searches == pirSlowPathAfter && w.fastest >= pirSlowPath
	}
	if w.recent = append(w.recent, info.RTT); len(w.recent) > pirFastRun {
		w.recent = w.recent[1:]
	}
	wasAsleep, known := w.asleep, w.known
	w.asleep, w.known = info.Asleep, true

	if !info.Asleep {
		// A client is connected: ours, or someone else's. Either way the
		// camera did not wake for motion, and when it sleeps again it
		// has to settle before a fast answer means anything.
		w.fast, w.slow, w.armed = 0, 0, false
		if known && wasAsleep && !ours && !w.other {
			w.other, w.otherSince = true, now
			return wakeOtherClient
		}
		if slowPath {
			return wakeSlowPath
		}
		return wakeNothing
	}

	verdict := wakeNothing
	if w.other {
		w.other = false
		verdict = wakeOtherGone
	}
	if info.RTT < pirFast {
		w.fast++
		w.slow = 0
		if w.fast >= pirFastRun && w.armed && !ours {
			w.armed = false
			return wakeMotion
		}
	} else {
		w.fast = 0
		if w.slow++; w.slow >= pirArmRun {
			w.armed = true
		}
	}
	if verdict == wakeNothing && slowPath {
		verdict = wakeSlowPath
	}
	return verdict
}

// SetMotionHook has f told when a battery camera's wake turns motion on
// (on) and when that motion ends, so it can go out like a wired camera's
// own alerts. f must not block.
func (s *Supervisor) SetMotionHook(f func(camera string, on bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.motionHook = f
}

func (s *Supervisor) hookMotion(camera string, on bool) {
	s.mu.Lock()
	f := s.motionHook
	s.mu.Unlock()
	if f != nil {
		f(camera, on)
	}
}

// watchWakes searches a battery camera every wakeWatchEvery and, when its
// PIR wakes it, wakes its main stream for wakeMotionHold, so whatever
// records the presence stream records the motion live, and tells the
// motion hook. Someone else connecting is logged, not reported as motion.
func (s *Supervisor) watchWakes(ctx context.Context, e *entry) {
	name := e.cfg.Name
	tick := time.NewTicker(wakeWatchEvery)
	defer tick.Stop()
	var w pirWatch
	var until time.Time // when the motion from the last wake ends; zero when none
	defer func() {
		if !until.IsZero() {
			s.hookMotion(name, false)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !until.IsZero() && time.Now().After(until) {
			until = time.Time{}
			s.hookMotion(name, false)
		}
		sctx, cancel := context.WithTimeout(ctx, wakeWatchTimeout)
		info, err := wakeSearch(sctx, e.cfg.Address)
		cancel()
		ours := false
		if err == nil {
			ours = wakeOurs(info.Addr, wakeOursGrace)
		}
		now := time.Now()
		switch w.see(now, info, err, ours) {
		case wakeOtherClient:
			log.Printf("reostream: %s: another client connected (the Reolink app, or another reostream); not motion", name)
			continue
		case wakeOtherGone:
			log.Printf("reostream: %s: the other client left; asleep again after %s", name, now.Sub(w.otherSince).Round(time.Second))
			continue
		case wakeSlowPath:
			log.Printf("reostream: %s: no search in %d came back faster than %v; motion from its own sensor shows only when it answers in under %v, so it will be missed from this far away",
				name, pirSlowPathAfter, w.fastest.Round(time.Millisecond), pirFast)
			continue
		case wakeMotion:
		default:
			continue
		}
		log.Printf("reostream: %s: motion: woke to record (searches answered in %s)", name, msList(w.recent))
		if until.IsZero() {
			s.hookMotion(name, true)
		}
		until = time.Now().Add(wakeMotionHold)
		if until.UnixNano() > e.motionUntil.Load() {
			e.motionUntil.Store(until.UnixNano())
		}
	}
}

// msList writes round trips as "4ms, 4ms, 5ms".
func msList(ds []time.Duration) string {
	b := make([]byte, 0, 32)
	for i, d := range ds {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, d.Round(time.Millisecond).String()...)
	}
	return string(b)
}
