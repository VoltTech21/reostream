package control

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// One connection per camera for the pan/tilt pad and presets, kept open
// between commands. A held arrow is a burst of commands a fraction of a
// second apart, and logging in for each one added 0.1 to 1 s to every move
// (a tester's E1 Zoom report, 10-27). It closes once the pad goes quiet,
// so it is not one more session the camera holds for nothing.

// ptzHoldIdle is how long the connection outlives the last command.
const ptzHoldIdle = 30 * time.Second

type ptzHold struct {
	mu    sync.Mutex
	conn  *baichuan.Conn
	timer *time.Timer
}

// withPtzConn runs fn on cam's held PTZ connection, dialling one if there is
// none. A command that fails on a held connection may have failed because
// the connection had gone stale, so that one is closed and fn tried once
// more on a fresh connection; a failure on a fresh one is the answer. A
// refusal is not a stale connection: the camera answered, so the
// connection is kept and the command is not sent again (see ptzRefusal).
func (s *Server) withPtzConn(ctx context.Context, cam Camera, fn func(*baichuan.Conn) error) error {
	s.ptzMu.Lock()
	if s.ptzHeld == nil {
		s.ptzHeld = map[string]*ptzHold{}
	}
	h := s.ptzHeld[cam.Name]
	if h == nil {
		h = &ptzHold{}
		s.ptzHeld[cam.Name] = h
	}
	s.ptzMu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.timer != nil {
		h.timer.Stop()
	}
	for try := 0; try < 2; try++ {
		fresh := false
		if h.conn == nil {
			conn, err := s.dial(ctx, cam)
			if err != nil {
				return err
			}
			h.conn, fresh = conn, true
		}
		err := fn(h.conn)
		var refused *baichuan.PtzRefused
		if err == nil || errors.As(err, &refused) {
			if err != nil {
				err = ptzRefusal(ctx, h.conn, refused)
			}
			h.timer = time.AfterFunc(ptzHoldIdle, func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.conn != nil {
					h.conn.Close()
					h.conn = nil
				}
			})
			return err
		}
		h.conn.Close()
		h.conn = nil
		if fresh || ctx.Err() != nil {
			return err
		}
	}
	return nil
}

// ptzRefusal says why the camera refused a PTZ command. A bare 400 is what
// a TrackMix answers to everything while its pan/tilt self check runs,
// after it starts and during a calibrate (fake hardware, 10-30), so the
// self test's state (340, testStat 1 while it runs) is read to say so.
func ptzRefusal(ctx context.Context, conn *baichuan.Conn, refused *baichuan.PtzRefused) error {
	if refused.Status != 400 {
		return refused
	}
	st, err := conn.PtzCalibrateState(ctx)
	if err == nil && st == 1 {
		return errors.New("the camera is running its pan/tilt self check (it does this after it starts and when calibrating) and takes no commands until it finishes, about a minute")
	}
	if err == nil {
		return errors.New("the camera refused the command (status 400), and it is not running its pan/tilt self check, so it does not take this command as sent")
	}
	return errors.New("the camera refused the command (status 400); a pan/tilt camera does this while it is still starting up or calibrating, so try again in a minute")
}
