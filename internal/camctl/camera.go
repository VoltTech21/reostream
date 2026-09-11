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
// classify. HungUp is set instead of any of them when the connection closed
// before this message got an answer at all, which is a fourth, real outcome
// docs/control.md's own probe table tracks separately ("hung up"): it is not
// the same fact as 405, it is the camera dropping the session rather than
// answering it.
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
// message, whatever its cause, is recorded against that message as a
// hang-up and the loop redials and continues, because the rest of the sweep
// is still worth having and a camera that drops one connection over one
// message has still told us something real about that message.
func (s *Server) probeCamera(ctx context.Context, cam Camera) ([]BlockProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		return nil, fmt.Errorf("camctl: probing %q: %w", cam.Name, err)
	}
	defer conn.Close()

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
