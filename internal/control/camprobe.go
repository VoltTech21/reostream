package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
)

// cameraProbeTimeout bounds one whole camera probe: baichuan.ConfigNames lists
// around a hundred reads, and a camera that hangs up partway through costs a
// reconnect on top of each one it drops. Without an outer bound a camera
// that keeps hanging up could hold this page open indefinitely; with it, a
// slow model still gets a full sweep, because 100 reads at readTimeout each
// fit comfortably inside it even in the worst case.
const cameraProbeTimeout = 3 * time.Minute

// readTimeout bounds a single config read. This is the other half of the
// bound above: cameraProbeTimeout stops the whole sweep from running forever, but
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

// newBlockProbe builds the BlockProbe for one answered read, classified by
// baichuan.ClassifyRead: status alone, not whether a body came back. See
// that function's comment for why a non-empty body is not the signal, and
// docs/control.md's probe table, which this and cmd/reocam's own probe
// command must now agree on.
func newBlockProbe(name string, id uint32, status int16, xml []byte) BlockProbe {
	bp := BlockProbe{Name: name, ID: id, Status: status}
	switch baichuan.ClassifyRead(status) {
	case baichuan.ReadSupported:
		bp.Supported = true
	case baichuan.ReadWantsParams:
		bp.WantsParams = true
	case baichuan.ReadAbsent:
		bp.Absent = true
	}
	return bp
}

// dial opens one connection to cam, using opts.Dial when the caller supplied
// one so tests can substitute a fake camera, and baichuan.Dial otherwise.
func (s *Server) dial(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
	// A camera that takes a connection and never answers (an old model
	// that serves one session at a time) costs a page every dial's full
	// timeout, and a page dials several times: 16 s for a camera's page,
	// 48 s for Advanced. One timeout is enough to know; for silentFor
	// after it the page's dials fail at once instead.
	key := fmt.Sprintf("%s/%s/%d", cam.Address, cam.UID, cam.Channel)
	s.silentMu.Lock()
	until := s.silentUntil[key]
	s.silentMu.Unlock()
	if time.Now().Before(until) {
		return nil, errCameraSilent
	}
	if _, ok := ctx.Deadline(); !ok {
		// A wired camera answers a login in well under a second; a
		// battery camera may first have to wake.
		wait := pageDialTimeout
		if cam.UID != "" {
			wait = pageDialTimeoutBattery
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	conn, err := s.dialCamera(ctx, cam)
	if err == nil {
		s.noteRecorder(cam.Address, conn)
	}
	s.silentMu.Lock()
	defer s.silentMu.Unlock()
	switch {
	case err == nil:
		delete(s.silentUntil, key)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		if s.silentUntil == nil {
			s.silentUntil = map[string]time.Time{}
		}
		s.silentUntil[key] = time.Now().Add(silentFor)
	}
	return conn, err
}

// pageDialTimeout bounds a dial from the page whose request set no deadline
// of its own.
var (
	pageDialTimeout        = 8 * time.Second
	pageDialTimeoutBattery = 20 * time.Second
)

// silentFor is how long a camera that timed out is not dialled again from
// the page.
var silentFor = 30 * time.Second

var errCameraSilent = errors.New("the camera is not answering (it timed out moments ago; trying again shortly)")

func (s *Server) dialCamera(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
	if s.opts.Dial != nil {
		return s.opts.Dial(ctx, cam)
	}
	return baichuan.Dial(ctx, cam.Address, baichuan.Options{
		Username: cam.Username,
		Password: cam.Password,
		UID:      cam.UID,
		Channel:  cam.Channel,
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
	ctx, cancel := context.WithTimeout(ctx, cameraProbeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		return nil, fmt.Errorf("control: probing %q: %w", cam.Name, err)
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
				return out, fmt.Errorf("control: probing %q: %w", cam.Name, ctx.Err())
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
				return out, fmt.Errorf("control: probing %q: reconnect after %s: %w", cam.Name, name, err)
			}
			continue
		}

		out = append(out, newBlockProbe(name, id, status, xml))
	}
	return out, nil
}

// isRecorderConn reports whether conn reached an NVR or Home Hub: every
// camera has "ipc" in its type at login, and a recorder does not.
func isRecorderConn(conn *baichuan.Conn) bool {
	if conn == nil {
		return false
	}
	di, err := baichuan.ParseDeviceInfo(conn.DeviceInfo())
	if err != nil {
		return false
	}
	// Cameras say "ipc", "fisheye_ipc" and the like, with typeInfo "IPC";
	// a recorder says neither.
	t := strings.ToLower(di.DeviceInfo.Type + " " + di.DeviceInfo.TypeInfo)
	return strings.TrimSpace(t) != "" && !strings.Contains(t, "ipc")
}

// secondLens reports a camera, not a recorder, with a zoom lens on
// channel 1, from its login reply. See baichuan.DeviceInfo.SecondLens.
func secondLens(conn *baichuan.Conn) bool {
	if conn == nil {
		return false
	}
	di, err := baichuan.ParseDeviceInfo(conn.DeviceInfo())
	return err == nil && di.SecondLens()
}

func (s *Server) noteRecorder(addr string, conn *baichuan.Conn) {
	rec := isRecorderConn(conn)
	s.recorderMu.Lock()
	defer s.recorderMu.Unlock()
	if s.recorders == nil {
		s.recorders = map[string]bool{}
	}
	s.recorders[config.NormalizeAddr(addr)] = rec
}

// behindRecorder reports whether cam is reached through an NVR or Home
// Hub. A channel other than 0 says so outright; channel 0 is a recorder's
// first camera as often as a camera on its own, so for that one it is
// what the address said it was the last time it was dialled, dialling
// once to find out if it never has been.
func (s *Server) behindRecorder(ctx context.Context, cam Camera) bool {
	if cam.Channel != 0 {
		return true
	}
	key := config.NormalizeAddr(cam.Address)
	s.recorderMu.Lock()
	rec, known := s.recorders[key]
	s.recorderMu.Unlock()
	if known {
		return rec
	}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return false
	}
	conn.Close()
	s.recorderMu.Lock()
	defer s.recorderMu.Unlock()
	return s.recorders[key]
}
