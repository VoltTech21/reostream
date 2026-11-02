package supervisor

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"time"
)

// A battery camera runs on its battery for months only if it sleeps. Its
// stream therefore runs only while a viewer is subscribed, and every limit
// below errs toward letting it sleep: a viewer who wants more presses
// play again.
var (
	// wakeIdle is how long a session outlives its last viewer, so a
	// reload or a switch between sub and main does not wake it twice.
	wakeIdle = 10 * time.Second
	// wakeMax caps one session however many viewers stay, so a tab left
	// open cannot hold the camera awake.
	wakeMax = 5 * time.Minute
	// wakeCooldown is the pause after a session ends before a viewer can
	// wake the camera again, so nothing reconnecting in a loop keeps it up.
	wakeCooldown = 5 * time.Second
	// wakeTries is how many failed wakes in a row are retried while a
	// viewer waits; after that the viewer is let go.
	wakeTries = 4
	// wakeRetry is the pause before retrying: the camera refuses a new
	// session for a moment after one closes, the camera page's own
	// settings read included.
	wakeRetry = time.Second
	// wakeIdleTimeout gives up on a session that logs in and sends
	// nothing (a held session) much sooner than a wired camera's default.
	wakeIdleTimeout = 4 * time.Second
	// wakePoll is how often demand is checked.
	wakePoll = 500 * time.Millisecond
	// motionHold is how long one report of motion keeps the camera awake
	// when no end of it arrives. reoevent sends its end 20 s after the
	// last motion, so this only matters if that message is lost.
	motionHold = 2 * time.Minute
)

// Motion tells a battery camera's main stream that the camera reports
// motion (on) or that the motion is over (off). While it lasts the stream
// wakes with nobody watching, so the presence stream, and whatever records
// it, carries the event live instead of the last still; wakeMax still caps
// the session. It reports whether camera is a battery camera with a main
// stream; a wired camera is never asleep, so it has nothing to do there.
func (s *Supervisor) Motion(camera string, on bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.onDemand && e.cfg.Name == camera && e.cfg.Stream == "main" {
			until := time.Now()
			if on {
				until = until.Add(motionHold)
			}
			e.motionUntil.Store(until.UnixNano())
			return true
		}
	}
	return false
}

// wanted is whether an on-demand stream should be awake: it is always on,
// someone is watching, or the camera reports motion.
func wanted(e *entry) bool {
	return e.alwaysOn || e.h.Clients() > 0 || time.Now().UnixNano() < e.motionUntil.Load()
}

// runOnDemand owns a battery camera's stream: asleep until a viewer
// subscribes, awake while one is, asleep again soon after the last leaves
// or when the session reaches wakeMax.
func (s *Supervisor) runOnDemand(ctx context.Context, e *entry) {
	defer e.h.Close()
	// An always-on camera never sleeps, so its waking says nothing.
	if e.cfg.Stream == "main" && !e.alwaysOn && e.cfg.Address != "" {
		// Waited for on the way out, so nothing of this camera's is still
		// running once its stream has stopped.
		wctx, stop := context.WithCancel(ctx)
		watched := make(chan struct{})
		go func() { defer close(watched); s.watchWakes(wctx, e) }()
		defer func() { stop(); <-watched }()
	}
	s.firstStill(ctx, e)
	tick := time.NewTicker(wakePoll)
	defer tick.Stop()
	for {
		// Asleep: wait for a viewer or motion.
		for !wanted(e) {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}

		fails := 0
		for fails < wakeTries && wanted(e) {
			reason, err := s.wakeSession(ctx, e)
			if ctx.Err() != nil {
				return
			}
			if err != nil && reason == "" {
				fails++
				s.noteFailure(e.cfg.Name, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(wakeRetry):
				}
				continue
			}
			s.noteRecovered(e.cfg.Name)
			log.Printf("reostream: %s: back to sleep (%s)", hubName(e.cfg.Name, e.cfg.Stream), reason)
			break
		}
		// Whoever is still subscribed is let go: after the cap, or after
		// the camera would not wake.
		e.h.Kick()
		s.saveStill(e)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wakeCooldown):
		}
	}
}

// wakeSession runs one session and reports why it ended: "" when the
// camera connection itself ended (err says why), otherwise the limit that
// ended it.
func (s *Supervisor) wakeSession(ctx context.Context, e *entry) (string, error) {
	sess, cancel := context.WithCancel(ctx)
	defer cancel()

	reason := make(chan string, 1)
	watched := make(chan struct{})
	defer func() { <-watched }()
	go func() {
		defer close(watched)
		tick := time.NewTicker(wakePoll)
		defer tick.Stop()
		start, lastSeen := time.Now(), time.Now()
		for {
			select {
			case <-sess.Done():
				return
			case <-tick.C:
			}
			if wanted(e) {
				lastSeen = time.Now()
			}
			switch {
			case time.Since(lastSeen) >= wakeIdle:
				reason <- "no one watching, no motion"
			case !e.alwaysOn && time.Since(start) >= wakeMax:
				reason <- "session limit"
			default:
				continue
			}
			cancel()
			return
		}
	}()

	why := "a viewer"
	switch {
	case e.alwaysOn:
		why = "always on"
	case e.h.Clients() == 0:
		why = "motion"
	}
	// Not repeated while the camera is failing: an always-on camera that
	// cannot be reached would otherwise say it every few seconds.
	if !s.failing(e.cfg.Name) {
		log.Printf("reostream: %s: waking for %s", hubName(e.cfg.Name, e.cfg.Stream), why)
	}
	s.setRunning(e, true)
	cfg := e.cfg
	cfg.IdleTimeout = wakeIdleTimeout
	err := s.run(sess, cfg, e.h)
	s.setRunning(e, false)
	cancel()
	select {
	case r := <-reason:
		return r, nil
	default:
	}
	if err != nil {
		s.setResult(e, err)
		return "", err
	}
	return "camera ended the session", nil
}

// SetStillDir keeps each battery camera's last picture in dir, so a restart
// does not leave its tile and its presence stream with nothing to show.
// Call before Run.
func (s *Supervisor) SetStillDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stillDir = dir
	for _, e := range s.entries {
		if e.onDemand {
			loadStill(dir, e)
		}
	}
}

func stillPath(dir string, e *entry) string {
	return filepath.Join(dir, e.cfg.Name+"_"+e.cfg.Stream+".still")
}

// A still file is the codec on its first line, then the keyframe's raw
// elementary stream.
func loadStill(dir string, e *entry) {
	if dir == "" {
		return
	}
	p := stillPath(dir, e)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	codec, es, ok := bytes.Cut(b, []byte("\n"))
	if !ok || len(es) == 0 {
		return
	}
	at := time.Now()
	if st, err := os.Stat(p); err == nil {
		at = st.ModTime()
	}
	e.h.RestoreKeyframe(string(codec), es, at)
}

func (s *Supervisor) saveStill(e *entry) {
	s.mu.Lock()
	dir := s.stillDir
	s.mu.Unlock()
	codec, es := e.h.Keyframe()
	if dir == "" || es == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := stillPath(dir, e) + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(codec+"\n"), es...), 0o644); err != nil {
		return
	}
	os.Rename(tmp, stillPath(dir, e))
}

var (
	// stillWait staggers main's first picture after sub's, so the two
	// never ask the camera for a session at the same moment.
	stillWait = map[string]time.Duration{"sub": 2 * time.Second, "main": 25 * time.Second}
	// stillTimeout bounds one wake for a picture.
	stillTimeout = 20 * time.Second
)

// firstStill wakes a battery camera once, briefly, for a stream that has no
// picture at all (none kept on disk either), so its tile and its presence
// stream have something to show. The session ends at the first keyframe.
// It never runs again once a picture exists.
func (s *Supervisor) firstStill(ctx context.Context, e *entry) {
	if _, es := e.h.Keyframe(); es != nil {
		return
	}
	s.mu.Lock()
	keep := s.stillDir != ""
	s.mu.Unlock()
	if !keep {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(stillWait[e.cfg.Stream]):
	}
	for try := 0; try < 3; try++ {
		sess, cancel := context.WithTimeout(ctx, stillTimeout)
		go func() {
			t := time.NewTicker(200 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-sess.Done():
					return
				case <-t.C:
					if _, es := e.h.Keyframe(); es != nil {
						cancel()
						return
					}
				}
			}
		}()
		cfg := e.cfg
		cfg.IdleTimeout = wakeIdleTimeout
		s.setRunning(e, true)
		s.run(sess, cfg, e.h)
		s.setRunning(e, false)
		cancel()
		if _, es := e.h.Keyframe(); es != nil {
			log.Printf("reostream: %s: took a first picture, back to sleep", hubName(e.cfg.Name, e.cfg.Stream))
			s.saveStill(e)
			return
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wakeRetry):
		}
	}
}
