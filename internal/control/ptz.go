package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
)

// ptzSpeed is the speed the Reolink client uses for a held arrow; the
// range is Reolink's documented 1-64.
const (
	ptzSpeed    = 34
	ptzSpeedMin = 1
	ptzSpeedMax = 64
)

// ptzView is what the Live tab's PTZ card and the Pan, tilt, zoom settings
// show.
type ptzView struct {
	Presets    []baichuan.Preset
	MaxPresets int
	PresetErr  string
	Guard      baichuan.PtzGuard
	GuardErr   string
	// Calibrated is the self test's state: "" unread, else "done",
	// "running" or "not run".
	Calibrated string
	// HasZoom is a camera that reports a zoom range (message 294);
	// HasPatrol one that says it patrols, or has presets saved to patrol
	// between (a TrackMix says ptzPatrol 0 and patrols all the same).
	HasZoom   bool
	HasPatrol bool
	// BehindRecorder hides auto return and calibrate, which a recorder
	// refuses (see notThroughRecorder).
	BehindRecorder bool
	// Unread is a battery camera's presets left unread so that opening
	// its page does not wake it; the card offers to read them.
	Unread   bool
	Speed    int
	SpeedMin int
	SpeedMax int
}

// NextPresetID is the lowest id no saved preset uses.
func (v ptzView) NextPresetID() int {
	used := map[int]bool{}
	for _, p := range v.Presets {
		used[p.ID] = true
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

func (s *Server) readPresets(ctx context.Context, cam Camera) ptzView {
	v := ptzView{Speed: ptzSpeed, SpeedMin: ptzSpeedMin, SpeedMax: ptzSpeedMax}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		v.PresetErr = fmt.Sprintf("could not connect: %v", err)
		return v
	}
	defer conn.Close()
	if v.Presets, v.MaxPresets, err = conn.PtzPresets(ctx); err != nil {
		v.PresetErr = err.Error()
	}
	if lo, hi, _, err := conn.PtzZoomRange(ctx); err == nil && hi > lo {
		v.HasZoom = true
	}
	return v
}

func (s *Server) readGuard(ctx context.Context, cam Camera) ptzView {
	v := ptzView{}
	if s.behindRecorder(ctx, cam) {
		v.BehindRecorder = true
		return v
	}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		v.GuardErr = fmt.Sprintf("could not connect: %v", err)
		return v
	}
	defer conn.Close()
	if v.Guard, err = conn.GetPtzGuard(ctx); err != nil {
		v.GuardErr = err.Error()
	}
	if st, err := conn.PtzCalibrateState(ctx); err == nil {
		v.Calibrated = "not run"
		switch st {
		case 1:
			v.Calibrated = "running"
		case 2:
			v.Calibrated = "done"
		}
	}
	return v
}

// ptzCam finds the camera a PTZ command is for and bounds the command,
// writing the failure as the answer.
func (s *Server) ptzCam(w http.ResponseWriter, r *http.Request) (Camera, context.Context, context.CancelFunc, bool) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return Camera{}, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout*2)
	return cam, ctx, cancel, true
}

// serveApplyPtz moves, zooms or stops the camera for the Live tab's pad, or
// starts or stops its patrol (command startPatrol or stopPatrol, patrol the
// patrol id, 0 when left out). It is a fetch target, not a form: 204 when
// the camera acknowledged the command, 502 with the reason when it did not.
func (s *Server) serveApplyPtz(w http.ResponseWriter, r *http.Request) {
	cmd := r.FormValue("command")
	patrol := 0
	switch cmd {
	case "left", "right", "up", "down", "leftUp", "leftDown", "rightUp", "rightDown", "zoomInc", "zoomDec", "stop":
	case "startPatrol", "stopPatrol":
		if v := r.FormValue("patrol"); v != "" {
			id, err := strconv.Atoi(v)
			if err != nil || id < 0 || id > 255 {
				http.Error(w, "bad patrol id", http.StatusBadRequest)
				return
			}
			patrol = id
		}
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
		return
	}
	speed := ptzSpeed
	if v, err := strconv.Atoi(r.FormValue("speed")); err == nil {
		speed = min(max(v, ptzSpeedMin), ptzSpeedMax)
	}
	cam, ctx, cancel, ok := s.ptzCam(w, r)
	if !ok {
		return
	}
	defer cancel()
	if err := s.withPtzConn(ctx, cam, func(conn *baichuan.Conn) error {
		if cmd == "startPatrol" || cmd == "stopPatrol" {
			return conn.PtzPatrol(ctx, cmd == "startPatrol", patrol)
		}
		return conn.PtzControl(ctx, cmd, speed)
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveApplyPreset goes to a saved position, or saves the current one.
// Same fetch contract as serveApplyPtz.
func (s *Server) serveApplyPreset(w http.ResponseWriter, r *http.Request) {
	op := r.FormValue("op")
	cmd := map[string]string{"goto": "toPos", "save": "setPos"}[op]
	if cmd == "" {
		http.Error(w, "unknown preset operation", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil || id < 0 || id > 255 {
		http.Error(w, "bad preset id", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if op == "save" && name == "" {
		name = fmt.Sprintf("Preset %d", id+1)
	}
	if len(name) > 32 {
		name = name[:32]
	}
	cam, ctx, cancel, ok := s.ptzCam(w, r)
	if !ok {
		return
	}
	defer cancel()
	if err := s.withPtzConn(ctx, cam, func(conn *baichuan.Conn) error {
		return conn.PtzPreset(ctx, cmd, id, name)
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notThroughRecorder is why auto return and calibrate are not offered for a
// camera reached through an NVR or Home Hub: a TrackMix on channel 1 of an
// RLN8-410 had the recorder answer 400 to the guard read (332) and to both
// calibrate messages (340, 341), from a user's report.
const notThroughRecorder = "is not available through a recorder: the NVR refuses it. Change it in the Reolink app, or add the camera by its own address."

// guardAnswerWait bounds the Baichuan auto return write. The fake TrackMix
// never answered 331 at all (10-30, bug 2), which cost the whole 15 s idle
// timeout before saying so. A var so a test can shorten it.
var guardAnswerWait = 5 * time.Second

// serveApplyGuard sets auto return: on or off, and after how many seconds,
// then reads it back to confirm. Over the HTTP API when the camera has it
// on (SetPtzGuard benable and timeout, which the fake TrackMix took and
// then acted on); otherwise the camera's own Baichuan guard document is
// echoed back with only those two fields changed, which that camera never
// answered, so it gets guardAnswerWait.
func (s *Server) serveApplyGuard(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	back := "/cameras/" + url.PathEscape(cam.Name) + "/settings/ptz"
	fail := func(msg string) {
		s.setFlash(w, r, Flash{Outcome: "refused", Message: cam.Name + ": auto return " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	if s.behindRecorder(ctx, cam) {
		fail(notThroughRecorder)
		return
	}
	// want applies the form to the camera's current values.
	want := func(enable bool, timeout int) (bool, int, error) {
		if v := r.FormValue("enable"); v != "" {
			enable = v == "1"
		}
		if v := r.FormValue("timeout"); v != "" {
			t, err := strconv.Atoi(v)
			if err != nil || t < 1 {
				return false, 0, fmt.Errorf("needs a number of seconds, not %q", v)
			}
			timeout = t
		}
		return enable, timeout, nil
	}
	done := func(enable bool, timeout int) {
		state := "off"
		if enable {
			state = fmt.Sprintf("on, after %d s", timeout)
		}
		s.setFlash(w, r, Flash{Outcome: "confirmed", Message: cam.Name + ": auto return " + state})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}

	if c, err := s.cgiDial(cam); err == nil {
		defer c.Close()
		curEnable, curTimeout, err := readGuardCGI(ctx, c, cam.Channel)
		if err != nil {
			fail(err.Error())
			return
		}
		enable, timeout, err := want(curEnable, curTimeout)
		if err != nil {
			fail(err.Error())
			return
		}
		bit := 0
		if enable {
			bit = 1
		}
		if err := c.Set(ctx, "SetPtzGuard", map[string]any{"PtzGuard": map[string]any{
			"channel": cam.Channel, "benable": bit, "timeout": timeout,
		}}); err != nil {
			fail(err.Error())
			return
		}
		gotEnable, gotTimeout, err := readGuardCGI(ctx, c, cam.Channel)
		if err != nil || gotEnable != enable || gotTimeout != timeout {
			fail("was accepted but the camera still reports it otherwise")
			return
		}
		done(enable, timeout)
		return
	}

	conn, err := s.dial(ctx, cam)
	if err != nil {
		fail("could not connect: " + err.Error())
		return
	}
	defer conn.Close()
	g, err := conn.GetPtzGuard(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	enable, timeout, err := want(g.Enable, g.Timeout)
	if err != nil {
		fail(err.Error())
		return
	}
	bit := "0"
	if enable {
		bit = "1"
	}
	doc := g.Doc
	if doc, err = setField(doc, "PtzGuard/benable", bit); err == nil {
		doc, err = setField(doc, "PtzGuard/timeout", strconv.Itoa(timeout))
	}
	if err != nil {
		fail(err.Error())
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, guardAnswerWait)
	err = conn.SetPtzGuard(wctx, doc)
	wcancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			fail(fmt.Sprintf("got no answer from the camera in %s. Turn on HTTP under Network so reostream can set it through the camera's web API.", guardAnswerWait))
			return
		}
		fail(err.Error())
		return
	}
	after, err := conn.GetPtzGuard(ctx)
	if err != nil || after.Enable != enable || after.Timeout != timeout {
		fail("was accepted but the camera still reports it otherwise")
		return
	}
	done(enable, timeout)
}

// readGuardCGI reads auto return's on/off and seconds over the HTTP API.
func readGuardCGI(ctx context.Context, c *cgi.Client, channel int) (enable bool, timeout int, err error) {
	value, _, _, err := c.Get(ctx, "GetPtzGuard", channel)
	if err != nil {
		return false, 0, err
	}
	var v struct {
		PtzGuard *struct {
			Enable  int `json:"benable"`
			Timeout int `json:"timeout"`
		} `json:"PtzGuard"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return false, 0, fmt.Errorf("control: parsing GetPtzGuard: %w", err)
	}
	if v.PtzGuard == nil {
		return false, 0, errors.New("control: GetPtzGuard carried no PtzGuard document")
	}
	return v.PtzGuard.Enable == 1, v.PtzGuard.Timeout, nil
}

// serveCalibrate starts the camera's pan/tilt self test: it moves through
// its whole range to find its limits, as the Reolink client's Calibrate.
// The camera holds its answer until the sweep ends (see PtzCalibrate), so
// the start is confirmed by reading the self test's state on a second
// connection: testStat 1 is running.
func (s *Server) serveCalibrate(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	back := "/cameras/" + url.PathEscape(cam.Name) + "/settings/ptz"
	flash := func(outcome, msg string) {
		s.setFlash(w, r, Flash{Outcome: outcome, Message: cam.Name + ": " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	if s.behindRecorder(ctx, cam) {
		flash("refused", "calibrate "+notThroughRecorder)
		return
	}
	conn, err := s.dial(ctx, cam)
	answered := false
	if err == nil {
		answered, err = conn.PtzCalibrate(ctx)
		conn.Close()
	}
	if err != nil {
		// It refuses a second start while one runs.
		if c2, derr := s.dial(ctx, cam); derr == nil {
			st, serr := c2.PtzCalibrateState(ctx)
			c2.Close()
			if serr == nil && st == 1 {
				flash("refused", "calibrate refused: the camera is already running its pan/tilt self check, about a minute")
				return
			}
		}
		flash("refused", "calibrate "+err.Error())
		return
	}
	st := -1
	if conn, err = s.dial(ctx, cam); err == nil {
		st, err = conn.PtzCalibrateState(ctx)
		conn.Close()
	}
	var refused *baichuan.PtzRefused
	switch {
	case errors.As(err, &refused):
		flash("refused", "calibrate "+err.Error())
	case err != nil:
		flash("accepted", "calibrate sent, but its state could not be read back: "+err.Error())
	case st == 1:
		flash("accepted", "calibrating, this takes a minute or two while the camera turns through its whole range")
	case st == 2 && answered:
		flash("confirmed", "calibrated")
	default:
		flash("accepted", "calibrate sent, but the camera does not report it running")
	}
}

// serveGuardPosition saves where the camera points now as its guard
// position (op=set), or sends it there (op=go). Over the HTTP API:
// SetPtzGuard's cmdStr setPos / toPos, as the camera's own web server
// names them; the Baichuan guard message has no such command.
func (s *Server) serveGuardPosition(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	back := "/cameras/" + url.PathEscape(cam.Name) + "/settings/ptz"
	op := r.FormValue("op")
	param := map[string]any{"channel": cam.Channel}
	what := ""
	switch op {
	case "set":
		param["cmdStr"], param["bSaveCurrentPos"] = "setPos", 1
		what = "guard position saved where it points now"
	case "go":
		param["cmdStr"] = "toPos"
		what = "going to the guard position"
	default:
		http.Error(w, "op is set or go", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	c, err := s.cgiDial(cam)
	if err == nil {
		defer c.Close()
		// Keep the camera's own enable and timeout in the same write.
		if value, _, _, gerr := c.Get(ctx, "GetPtzGuard", cam.Channel); gerr == nil {
			var cur struct {
				PtzGuard map[string]any `json:"PtzGuard"`
			}
			if json.Unmarshal(value, &cur) == nil {
				for _, k := range []string{"benable", "timeout"} {
					if v, ok := cur.PtzGuard[k]; ok {
						param[k] = v
					}
				}
			}
		}
		err = c.Set(ctx, "SetPtzGuard", map[string]any{"PtzGuard": param})
		c.Close()
	}
	if err != nil {
		s.setFlash(w, r, Flash{Outcome: "refused", Message: cam.Name + ": " + err.Error()})
	} else {
		s.setFlash(w, r, Flash{Outcome: "accepted", Message: cam.Name + ": " + what})
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
