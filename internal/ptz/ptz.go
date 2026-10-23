// Package ptz drives a camera's motors over Baichuan on behalf of a
// protocol front end such as internal/onvif.
//
// The streaming connections are never used for this. A Controller keeps its
// own session, opened on the first command and closed again after a quiet
// spell, so a camera nobody is steering costs one connection only while a
// person is actually using it. Commands are serialised: the camera has one
// set of motors and the last word wins.
package ptz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Timing defaults. The idle value is a trade between reconnecting for every
// burst of button presses and holding a camera session for no reason.
const (
	DefaultIdle    = 30 * time.Second
	DefaultMaxMove = 15 * time.Second
	opTimeout      = 8 * time.Second
)

// Session is one open conversation with the camera. It is an interface so
// the Controller's timing and retry rules can be tested without a camera.
type Session interface {
	Control(ctx context.Context, cmd baichuan.PTZCommand, speed int) (int16, error)
	Presets(ctx context.Context) ([]baichuan.PTZPreset, error)
	GotoPreset(ctx context.Context, id int) (int16, error)
	Close() error
}

// Dial opens one session to the camera.
type Dial func(ctx context.Context) (Session, error)

// FromBaichuan adapts a function that dials a Baichuan connection.
func FromBaichuan(dial func(ctx context.Context) (*baichuan.Conn, error)) Dial {
	return func(ctx context.Context) (Session, error) {
		conn, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		return baichuanSession{conn}, nil
	}
}

type baichuanSession struct{ conn *baichuan.Conn }

func (s baichuanSession) Control(ctx context.Context, cmd baichuan.PTZCommand, speed int) (int16, error) {
	return baichuan.PTZControl(ctx, s.conn, cmd, speed)
}

func (s baichuanSession) Presets(ctx context.Context) ([]baichuan.PTZPreset, error) {
	return baichuan.ReadPTZPresets(ctx, s.conn)
}

func (s baichuanSession) GotoPreset(ctx context.Context, id int) (int16, error) {
	return baichuan.PTZGotoPreset(ctx, s.conn, id)
}

func (s baichuanSession) Close() error { return s.conn.Close() }

// Controller is the PTZ state of one camera.
type Controller struct {
	dial    Dial
	idle    time.Duration
	maxMove time.Duration

	mu      sync.Mutex
	conn    Session
	idleT   *time.Timer
	moveT   *time.Timer
	moving  bool
	closed  bool
	presets []baichuan.PTZPreset
}

// New returns a Controller. Zero idle or maxMove take the defaults.
func New(dial Dial, idle, maxMove time.Duration) *Controller {
	if idle <= 0 {
		idle = DefaultIdle
	}
	if maxMove <= 0 {
		maxMove = DefaultMaxMove
	}
	return &Controller{dial: dial, idle: idle, maxMove: maxMove}
}

// Moving reports whether a movement was started and not yet stopped.
func (c *Controller) Moving() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moving
}

// Move starts a movement that runs until Stop, or until maxMove has passed:
// a controller that crashes or loses its network mid-press must not leave a
// motor driving into its end stop.
func (c *Controller) Move(ctx context.Context, cmd baichuan.PTZCommand, speed int) error {
	if cmd == baichuan.PTZStop {
		return c.Stop(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.do(ctx, func(ctx context.Context, conn Session) error {
		st, err := conn.Control(ctx, cmd, speed)
		return expect200("ptz move", st, err)
	}); err != nil {
		return err
	}
	c.moving = true
	if c.moveT != nil {
		c.moveT.Stop()
	}
	c.moveT = time.AfterFunc(c.maxMove, func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return nil
}

// Stop halts any movement.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.moveT != nil {
		c.moveT.Stop()
		c.moveT = nil
	}
	if !c.moving && c.conn == nil {
		// Nothing was started and no session is open to have started it,
		// so there is nothing to stop and no reason to wake the camera.
		return nil
	}
	err := c.do(ctx, func(ctx context.Context, conn Session) error {
		st, err := conn.Control(ctx, baichuan.PTZStop, 1)
		return expect200("ptz stop", st, err)
	})
	c.moving = false
	return err
}

// Presets lists the camera's saved positions.
func (c *Controller) Presets(ctx context.Context) ([]baichuan.PTZPreset, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.do(ctx, func(ctx context.Context, conn Session) error {
		ps, err := conn.Presets(ctx)
		if err != nil {
			return err
		}
		c.presets = ps
		return nil
	})
	return append([]baichuan.PTZPreset(nil), c.presets...), err
}

// GotoPreset moves to a saved position.
func (c *Controller) GotoPreset(ctx context.Context, id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.moveT != nil {
		c.moveT.Stop()
		c.moveT = nil
	}
	c.moving = false
	return c.do(ctx, func(ctx context.Context, conn Session) error {
		st, err := conn.GotoPreset(ctx, id)
		return expect200("ptz preset", st, err)
	})
}

// Close ends the session and cancels anything pending. The Controller can
// not be used afterwards.
func (c *Controller) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.moveT != nil {
		c.moveT.Stop()
	}
	c.dropLocked()
}

var errClosed = errors.New("ptz: controller closed")

// do runs op on the session, opening it if need be, and tries once more on a
// fresh session when the first attempt fails: a session the camera dropped
// while idle looks fine until the first write. c.mu must be held.
func (c *Controller) do(ctx context.Context, op func(context.Context, Session) error) error {
	if c.closed {
		return errClosed
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if c.conn == nil {
			if c.conn, err = c.dial(ctx); err != nil {
				c.conn = nil
				return fmt.Errorf("ptz: connect: %w", err)
			}
		}
		if err = op(ctx, c.conn); err == nil {
			c.armIdleLocked()
			return nil
		}
		c.dropLocked()
		if ctx.Err() != nil {
			break
		}
	}
	return err
}

func (c *Controller) armIdleLocked() {
	if c.idleT != nil {
		c.idleT.Stop()
	}
	c.idleT = time.AfterFunc(c.idle, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.moving {
			c.armIdleLocked()
			return
		}
		c.dropLocked()
	})
}

// dropLocked closes the session, which tells the camera to release it.
func (c *Controller) dropLocked() {
	if c.idleT != nil {
		c.idleT.Stop()
		c.idleT = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func expect200(what string, status int16, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if status != 200 {
		return fmt.Errorf("%s: camera answered status %d", what, status)
	}
	return nil
}
