package supervisor

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A camera that cannot be reached fails on every stream every few seconds,
// and logging each attempt buried everything else: one morning's wrong
// password was six cameras times three streams every fifteen seconds, over
// a thousand identical lines an hour. So a camera's failure is logged once,
// in words an operator can act on, then reminded every failRemind while it
// lasts, and the camera coming back is logged once too. The status endpoint
// still carries every stream's last error for anyone who wants the detail.

// failRemind is how often a camera that is still failing is mentioned.
var failRemind = 10 * time.Minute

type camFailure struct {
	since   time.Time
	msg     string
	tries   int
	lastLog time.Time
}

// plainError says what is wrong in the terms someone fixing it would use.
func plainError(err error) string {
	s := err.Error()
	switch {
	case errors.Is(err, baichuan.ErrUnauthorized):
		msg := "wrong username or password (a Reolink NVR or Home Hub that added this camera may have changed it)"
		if i := strings.LastIndex(s, ", "); i >= 0 && strings.Contains(s[i:], "tries left") {
			msg += "; " + strings.TrimSuffix(s[i+2:], ")")
		}
		return msg
	case errors.Is(err, baichuan.ErrLoginRefused):
		return "the camera is refusing logins for now, often for a minute or two after many logins"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "connection refused"),
		strings.Contains(s, "network is unreachable"), strings.Contains(s, "no answer for uid"):
		return "can't reach the camera (" + lastPart(s) + ")"
	}
	return s
}

// lastPart is the innermost reason of a wrapped error, without the chain
// of "stream: x: dial: baichuan: dial ..." prefixes.
func lastPart(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

// noteFailure records one failed attempt for camera and logs it if it is the
// first, a different reason from before, or failRemind since the last word.
func (s *Supervisor) noteFailure(camera string, err error) {
	if err == nil {
		return
	}
	msg := plainError(err)
	now := time.Now()
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.failures == nil {
		s.failures = map[string]*camFailure{}
	}
	f := s.failures[camera]
	switch {
	case f == nil:
		s.failures[camera] = &camFailure{since: now, msg: msg, tries: 1, lastLog: now}
		log.Printf("reostream: %s: %s; retrying", camera, msg)
	case f.msg != msg:
		f.msg, f.lastLog = msg, now
		f.tries++
		log.Printf("reostream: %s: now %s; retrying", camera, msg)
	default:
		f.tries++
		if now.Sub(f.lastLog) >= failRemind {
			f.lastLog = now
			log.Printf("reostream: %s: still failing after %d tries over %s: %s", camera, f.tries, roundDur(now.Sub(f.since)), msg)
		}
	}
}

// noteRecovered logs a failing camera coming back, once.
func (s *Supervisor) noteRecovered(camera string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	f := s.failures[camera]
	if f == nil {
		return
	}
	delete(s.failures, camera)
	log.Printf("reostream: %s: back after %s", camera, roundDur(time.Since(f.since)))
}

// failing reports whether camera is currently failing.
func (s *Supervisor) failing(camera string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return s.failures[camera] != nil
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
