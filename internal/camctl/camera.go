package camctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// probeTimeout bounds one whole camera probe: baichuan.ConfigNames lists
// around a hundred reads, and a camera that hangs up partway through costs a
// reconnect on top of each one it drops. Without an outer bound a camera
// that keeps hanging up could hold this page open indefinitely; with it, a
// slow model still gets a full sweep, because 100 reads at readTimeout each
// fit comfortably inside it even in the worst case.
const probeTimeout = 3 * time.Minute

// readTimeout bounds a single config read. This is the other half of the
// bound above: probeTimeout stops the whole sweep from running forever, but
// without a per-read deadline too, one message a camera silently ignores
// (as opposed to answering 405 for, which is instant) could by itself eat
// the entire probe budget and starve every read after it.
const readTimeout = 4 * time.Second

// BlockProbe is what one config read established about a camera.
//
// Supported, WantsParams and Absent are mutually exclusive and come from
// classify. HungUp is set instead of any of them when this message got no
// answer at all, which is a fourth, real outcome docs/control.md's own probe
// table tracks separately from 405 ("hung up"): the camera dropping the
// session, or simply staying silent past readTimeout, is not the same fact
// as answering 405. probeCamera cannot always tell those two apart (see its
// own comment on the error branch that sets this), so a rendered page
// should describe HungUp as "no answer" rather than claim a disconnect it
// did not necessarily observe.
type BlockProbe struct {
	Name        string
	ID          uint32
	Status      int16
	Supported   bool
	WantsParams bool
	Absent      bool
	HungUp      bool
}

// classify sorts one config read's reply into the three answers a camera can
// give about a message it was asked for. A camera that does not implement a
// message answers 405 rather than dropping the connection, which is what
// makes asking about every known message safe, and is the only honest way
// to learn what a model has: no table can say it in advance.
func classify(status int16, xml []byte) string {
	switch status {
	case baichuan.StatusNotImplemented:
		return "absent"
	case baichuan.StatusBadRequest:
		return "wants params"
	default:
		return "supported"
	}
}

// newBlockProbe builds the BlockProbe for one answered read.
func newBlockProbe(name string, id uint32, status int16, xml []byte) BlockProbe {
	bp := BlockProbe{Name: name, ID: id, Status: status}
	switch classify(status, xml) {
	case "supported":
		bp.Supported = true
	case "wants params":
		bp.WantsParams = true
	case "absent":
		bp.Absent = true
	}
	return bp
}

// dial opens one connection to cam, using opts.Dial when the caller supplied
// one so tests can substitute a fake camera, and baichuan.Dial otherwise.
func (s *Server) dial(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
	if s.opts.Dial != nil {
		return s.opts.Dial(ctx, cam)
	}
	return baichuan.Dial(ctx, cam.Address, baichuan.Options{
		Username: cam.Username,
		Password: cam.Password,
	})
}

// probeCamera dials cam once and asks it for every config message
// baichuan.ConfigNames knows, recording how it answered each one.
//
// Some messages make a camera hang up rather than answer, which
// cmd/reocam's own sweep (cmd/reocam/main.go's get "all") already handles
// by starting a fresh connection partway through and carrying on rather
// than stopping the sweep there. This does the same: an error reading one
// message, whatever its cause, is recorded against that message as no
// answer and the loop redials and continues, because the rest of the sweep
// is still worth having and a camera that drops one connection over one
// message has still told us something real about that message.
func (s *Server) probeCamera(ctx context.Context, cam Camera) ([]BlockProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		return nil, fmt.Errorf("camctl: probing %q: %w", cam.Name, err)
	}
	// conn is reassigned on every redial below, so this cannot be
	// `defer conn.Close()`: that would bind to the connection conn holds
	// right now, at the point the defer statement runs, and go on closing
	// that stale one even after conn is replaced. A closure re-reads conn
	// at return time instead, so whichever connection is current gets
	// closed, and only that one. An unclosed connection does not release
	// the camera's session, and a camera holding a session refuses new
	// connections for minutes, so leaking one here is not a benign leak.
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	names := baichuan.ConfigNames()
	out := make([]BlockProbe, 0, len(names))
	for _, name := range names {
		id := baichuan.ConfigMessages[name]

		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		xml, status, err := baichuan.ReadConfig(readCtx, conn, id)
		readCancel()

		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
				// The whole probe's budget is gone, not just this one
				// read's. Report what was learned rather than losing it
				// to a caller that only sees the error.
				return out, fmt.Errorf("camctl: probing %q: %w", cam.Name, ctx.Err())
			}
			// ReadConfig's error does not say whether the camera dropped
			// the connection or just stayed silent past readTimeout: both
			// surface as this same err. Either way this message got no
			// answer, so both are recorded as HungUp and the loop redials
			// to be safe rather than keep using a connection that might
			// already be dead.
			out = append(out, BlockProbe{Name: name, ID: id, HungUp: true})
			conn.Close()
			conn, err = s.dial(ctx, cam)
			if err != nil {
				return out, fmt.Errorf("camctl: probing %q: reconnect after %s: %w", cam.Name, name, err)
			}
			continue
		}

		out = append(out, newBlockProbe(name, id, status, xml))
	}
	return out, nil
}
