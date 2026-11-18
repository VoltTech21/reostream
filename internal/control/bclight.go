package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
)

// The floodlight and the siren over Baichuan, for a camera with no web API.
//
// An original Lumus serves nothing but its media port (GitHub #29), and a
// battery camera has no web API at all, so the CGI floodlight in lights.go
// cannot reach either. Over Baichuan the light has a different shape: 288
// lights it or puts it out now, and leaves alone what it does by itself
// (FloodlightTask, 289: off, on motion or on a schedule), where the CGI
// write sets that mode. So:
//
//   - A camera whose web API answers keeps the CGI floodlight, with its four
//     options, exactly as before. Nothing changes for it.
//   - A camera whose web API cannot be reached at all, and every battery
//     camera, gets the light over Baichuan: on and off now, with its own
//     mode shown alongside, read from 289.
//   - A web API that answers and refuses the login is reported as it is,
//     and not quietly routed round: that is a wrong password to fix.
//
// The siren is Baichuan on every camera already (263, the sound test), and
// siren on detection is the AudioTask block (232/231).

// floodlightBCWait is how long a Baichuan read waits for the floodlight
// report a camera pushes after login. A TrackMix sends it within half a
// second; a Lumus with the login reply itself.
var floodlightBCWait = 2 * time.Second

// lightWebAPI decides how cam's floodlight is reached. It returns an open
// CGI client when the web API answers (the caller closes it), viaBaichuan
// when there is no web API to use, or the web API's own refusal.
func (s *Server) lightWebAPI(cam Camera) (c *cgi.Client, viaBaichuan bool, err error) {
	if cam.UID != "" {
		return nil, true, nil
	}
	c, err = s.cgiDial(cam)
	if err == nil {
		return c, false, nil
	}
	if cgi.Unreachable(err) {
		return nil, true, nil
	}
	return nil, false, err
}

// bcLight is what a Baichuan read of the floodlight found.
type bcLight struct {
	// Known says the camera reported the light's state; Lit is that state.
	Known bool
	Lit   bool
	// Mode is what the light does by itself (floodlightState's names:
	// off, motion, schedule), "" when 289 did not answer.
	Mode string
}

// bcFloodlightOptions is what the lights page offers over the media port:
// the light now, on or off.
var bcFloodlightOptions = []floodlightOption{{Value: "off"}, {Value: "on", Mode: 1, State: 1}}

// bcLightState is the option a Baichuan read matches, "" when the camera
// sent no report.
func bcLightState(l bcLight) string {
	if !l.Known {
		return ""
	}
	return onOff(l.Lit)
}

// floodlightTaskMode names a FloodlightTask alarmMode the way the CGI
// options do. docs/control.md's table: 0 off, 1 lights on detection, 3 the
// schedule.
func floodlightTaskMode(doc []byte) string {
	v, ok := fieldValue(doc, "FloodlightTask/alarmMode")
	if !ok {
		return ""
	}
	switch v {
	case "0":
		return "off"
	case "1", "2":
		return "motion"
	case "3":
		return "schedule"
	}
	return "mode " + v
}

// readFloodlightBC reads the light on a connection of its own: the report
// pushed after login first, before any other read can pass it by, then
// the task block for its mode.
func (s *Server) readFloodlightBC(ctx context.Context, cam Camera) (bcLight, error) {
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return bcLight{}, err
	}
	defer conn.Close()
	var out bcLight
	st, ok, err := conn.FloodlightNow(ctx, floodlightBCWait)
	if err != nil {
		return bcLight{}, err
	}
	out.Known, out.Lit = ok, ok && st.On()
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(rctx, conn, baichuan.MsgIDFloodlightTaskGet)
	cancel()
	if err == nil && status == 200 {
		out.Mode = floodlightTaskMode(doc)
	}
	return out, nil
}

// setFloodlightBC lights cam's light or puts it out over Baichuan and
// reports what the camera's floodlight report then said.
func (s *Server) setFloodlightBC(ctx context.Context, cam Camera, on bool) (baichuan.FloodlightStatus, bool, error) {
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return baichuan.FloodlightStatus{}, false, err
	}
	defer conn.Close()
	st, reported, err := conn.SetFloodlight(ctx, on, floodlightSettle)
	if err == nil {
		log.Printf("control: %s: floodlight %s over the media port; report: %v", cam.Name, onOff(on), reportWord(st, reported))
	}
	return st, reported, err
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func reportWord(st baichuan.FloodlightStatus, reported bool) string {
	if !reported {
		return "none"
	}
	return fmt.Sprintf("%s, brightness %d", onOff(st.On()), st.Brightness)
}

// applyFloodlightBC is serveApplyFloodlight's Baichuan half. "on" lights
// the light; every other option puts it out, which over Baichuan leaves
// the light doing what it does by itself, so motion and schedule are the
// same command as off and say so.
func (s *Server) applyFloodlightBC(ctx context.Context, cam Camera, opt floodlightOption) WriteResult {
	on := opt.Value == "on"
	st, reported, err := s.setFloodlightBC(ctx, cam, on)
	result := WriteResult{}
	switch {
	case err != nil:
		result.Outcome = "refused"
		result.Detail = err.Error()
	case !reported:
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command over its media port but sent no floodlight report within %s. This is not proof the light changed.", floodlightSettle)
	case st.On() == on:
		result.Outcome = "confirmed"
		result.After = []byte(onOff(on))
		if !on && opt.Value != "off" {
			result.Detail = "out now; this camera has no web API, so what the light does by itself was left as it was"
		}
	default:
		result.Outcome = "accepted"
		result.After = []byte(onOff(st.On()))
		result.Detail = fmt.Sprintf("the camera answered success, but its floodlight report still says %s.", onOff(st.On()))
	}
	return result
}

// soundSiren plays the siren once on cam.
func (s *Server) soundSiren(ctx context.Context, cam Camera) error {
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.SoundSiren(ctx)
}

// serveSoundSiren is the Live tab's siren button and the control API's:
// one press, one siren, about 3 seconds, which stops by itself. It answers
// JSON, as the sound test does.
func (s *Server) serveSoundSiren(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	if err := s.soundSiren(ctx, cam); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	log.Printf("control: %s: siren sounded", cam.Name)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "detail": "sounding"})
}

// setSirenOnDetection turns siren on detection on or off: the AudioTask
// block (232) read, its enable changed, and written back whole (231), as
// the app does, then read back on a fresh connection.
func (s *Server) setSirenOnDetection(ctx context.Context, cam Camera, on bool) (WriteResult, error) {
	pair, ok := pairForName("audio task get")
	if !ok {
		return WriteResult{}, fmt.Errorf("control: no audio task pair")
	}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return WriteResult{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(rctx, conn, pair.Get)
	cancel()
	conn.Close()
	if err != nil {
		return WriteResult{}, err
	}
	if status != 200 {
		return WriteResult{}, fmt.Errorf("control: reading siren on detection: %s", baichuan.ExplainStatus(status))
	}
	want := "0"
	if on {
		want = "1"
	}
	if cur, _ := fieldValue(doc, "AudioTask/enable"); cur == want {
		return WriteResult{Outcome: "confirmed", Detail: "already " + onOff(on)}, nil
	}
	body, err := setField(doc, "AudioTask/enable", want)
	if err != nil {
		return WriteResult{}, err
	}
	return s.writeBlock(ctx, cam, pair.Set, body, true)
}

// sirenOnDetection reads whether siren on detection is on.
func (s *Server) sirenOnDetection(ctx context.Context, cam Camera) (bool, error) {
	pair, ok := pairForName("audio task get")
	if !ok {
		return false, fmt.Errorf("control: no audio task pair")
	}
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	doc, status, err := baichuan.ReadConfig(rctx, conn, pair.Get)
	if err != nil {
		return false, err
	}
	if status != 200 {
		return false, fmt.Errorf("control: reading siren on detection: %s", baichuan.ExplainStatus(status))
	}
	v, ok := fieldValue(doc, "AudioTask/enable")
	if !ok {
		return false, fmt.Errorf("control: the audio task carries no enable")
	}
	return v != "0", nil
}
