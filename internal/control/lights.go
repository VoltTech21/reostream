// Lights and IR: the curated controls whose write is not a document field
// but a physical effect a person or a camera can see happen. That is what
// separates this file from settings.go's own groups: every control here
// confirms before acting, every time, not once per session, because a light
// switching on in a shop at two in the morning should never be a stray
// click.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// lightsConfirmReason is the reason shown next to every control in the
// Lights and IR group, and used as the confirmation prompt each control's
// form asks before it submits. It belongs in the template as much as in
// this comment: a person choosing "on" needs to read this before the click
// happens, not discover it afterward in a log.
const lightsConfirmReason = "This changes a physical light on the camera right now, not just a document. Are you sure?"

// mode and state are different things, and conflating them makes the light
// look as though it only has an on switch. mode is what the light does;
// state is whether it is lit right now. See docs/control.md, "The
// floodlight, as a worked example": mode 1 with state 1 is "on", mode 1
// with state 0 is "motion", which is how these cameras ship.
//
// GetWhiteLed and SetWhiteLed carry a real WhiteLed document over CGI, and
// this codebase does not model all of it: bright and LightingSchedule are
// both real fields a camera sends that no struct here names. See
// setFloodlight for why that means mode and state are read as a
// map[string]any rather than a narrow struct.

// floodlightOption is one choice the settings page offers for the
// floodlight: a name, and the (mode, state) pair over CGI that produces it.
//
// CGI modes 1 and 2 both read back as FloodlightTask alarmMode 1, enable 1
// ("lights on detection"), so only the canonical mode 1 is offered here;
// nothing distinguishes 2 from 1 in what a person would choose.
type floodlightOption struct {
	Value string
	Mode  int
	State int
}

// floodlightOptions is every choice the settings page offers, in the order
// docs/control.md's floodlight table lists them.
var floodlightOptions = []floodlightOption{
	{Value: "off", Mode: 0, State: 0},
	{Value: "on", Mode: 1, State: 1},
	{Value: "motion", Mode: 1, State: 0},
	{Value: "schedule", Mode: 3, State: 0},
}

// floodlightOptionByValue finds the option a form posted, refusing an
// unrecognised value rather than passing it through to the camera.
func floodlightOptionByValue(value string) (floodlightOption, bool) {
	for _, o := range floodlightOptions {
		if o.Value == value {
			return o, true
		}
	}
	return floodlightOption{}, false
}

// floodlightState names what a (mode, state) pair means, the same mapping
// docs/control.md's floodlight table records, established by setting each
// CGI mode and reading the Baichuan FloodlightTask back.
func floodlightState(mode, state int) string {
	switch mode {
	case 0:
		return "off"
	case 3:
		return "schedule"
	case 1, 2:
		if state != 0 {
			return "on"
		}
		return "motion"
	default:
		return fmt.Sprintf("unknown mode %d", mode)
	}
}

// cgiDial opens a CGI session to cam, using opts.CGIDial when the caller
// supplied one so a test can substitute a fake HTTP server, and cgi.Dial
// otherwise. This mirrors dial in camera.go, which does the same for
// Baichuan: the floodlight is reachable only over CGI, never over Baichuan,
// so it needs its own transport and its own substitution point.
//
// This does not take ctx, on purpose, even though every caller now has one
// in hand: Options.CGIDial is the substitution point every test in this
// package already builds against with the two-argument (cam Camera) shape,
// and widening it to take ctx too would mean rewriting every one of those
// test doubles for a dial that already carries its own 20 second timeout
// and is not part of the repeated read/write/read-back chain the fleet
// timeout actually needs to bound. See cgi.Client.Get and Set, which do
// take ctx, for the calls that chain matters for.
func (s *Server) cgiDial(cam Camera) (*cgi.Client, error) {
	if s.opts.CGIDial != nil {
		return s.opts.CGIDial(cam)
	}
	return cgi.Dial(cam.Address, cam.Username, cam.Password)
}

// readFloodlightDoc reads the floodlight's whole GetWhiteLed document as a
// generic map rather than a narrow struct, the same discipline
// fleetapply.go's readFullTime already uses for GetTime: a real WhiteLed
// object carries bright and LightingSchedule, fields this codebase does not
// model, and setFloodlight changes only mode and state on top of whatever
// this returns rather than composing a document that drops them.
//
// GetWhiteLed intermittently answers "please login first" on a connection
// that logged in seconds earlier; c already logs in again and retries once,
// so nothing here adds a second retry layer on top of it.
func readFloodlightDoc(ctx context.Context, c *cgi.Client) (map[string]any, error) {
	value, _, _, err := c.Get(ctx, "GetWhiteLed", 0)
	if err != nil {
		return nil, err
	}
	var v struct {
		WhiteLed map[string]any `json:"WhiteLed"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, fmt.Errorf("control: parsing GetWhiteLed: %w", err)
	}
	if v.WhiteLed == nil {
		return nil, fmt.Errorf("control: GetWhiteLed carried no WhiteLed document")
	}
	return v.WhiteLed, nil
}

// floodlightModeState reads mode and state off a WhiteLed document, the
// only two fields this page curates a control for.
func floodlightModeState(doc map[string]any) (mode, state int) {
	if f, ok := doc["mode"].(float64); ok {
		mode = int(f)
	}
	if f, ok := doc["state"].(float64); ok {
		state = int(f)
	}
	return mode, state
}

// readFloodlight reads the floodlight's current mode and state over CGI,
// for display: serveSettings only ever shows floodlightState(mode, state),
// never the raw document, so it keeps this narrow entry point.
func readFloodlight(ctx context.Context, c *cgi.Client) (mode, state int, err error) {
	doc, err := readFloodlightDoc(ctx, c)
	if err != nil {
		return 0, 0, err
	}
	mode, state = floodlightModeState(doc)
	return mode, state, nil
}

// setFloodlight sends mode and state over CGI, changed on top of the
// camera's current WhiteLed document rather than composed from nothing.
//
// This is the floodlight's only working write path. Its Baichuan
// equivalent, message 288, is known inert on this firmware: it takes a
// document built with the correct element name, taken from the firmware
// rather than guessed, and answers 200 while changing nothing observable. A
// wrong element name on the same message answers 400, so the document is
// parsed and then discarded, not merely unsupported. Routing the floodlight
// through writeBlock because it looks tidier next to the rest of this
// codebase would not work.
//
// The body posted here is readFloodlightDoc's own reply with mode and state
// overwritten, never a three-field object built from scratch: the camera
// supplies its own schema, including bright and LightingSchedule, and a
// composed document drops both. That is what made the "schedule" option
// (mode 3) send no schedule at all before this changed.
func setFloodlight(ctx context.Context, c *cgi.Client, mode, state int) error {
	cur, err := readFloodlightDoc(ctx, c)
	if err != nil {
		return err
	}
	cur["mode"] = mode
	cur["state"] = state
	cur["channel"] = 0
	return c.Set(ctx, "SetWhiteLed", map[string]any{"WhiteLed": cur})
}

// serveApplyFloodlight is the floodlight's POST: set the requested option
// over CGI, then read GetWhiteLed back on the same session to see whether
// it actually took.
//
// The read-back this handler takes is on the write connection, not a fresh
// one: writeBlock's fresh-connection discipline exists because a camera can
// answer a same-session read from state it has not committed, and the
// floodlight's CGI read-back has exactly that problem, with no fresh-session
// equivalent available over CGI the way writeBlock has one over Baichuan.
// So this can claim no more than "accepted": the camera reported the
// change, but the effect is not verified from here. See docs/control.md.
//
// The confirmation dialog itself is not this handler's job. It runs
// client-side, in the template, before the browser ever issues this
// request; by the time this runs, the person has already been asked.
func (s *Server) serveApplyFloodlight(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	opt, ok := floodlightOptionByValue(r.FormValue("option"))
	if !ok {
		http.Error(w, fmt.Sprintf("%q is not a floodlight option", r.FormValue("option")), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	c, err := s.cgiDial(cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("connecting to %s: %v", cam.Name, err), http.StatusBadGateway)
		return
	}

	// Best effort: a failed pre-read is not a reason to refuse the write
	// itself, only to leave Before blank.
	var before string
	if beforeMode, beforeState, readErr := readFloodlight(ctx, c); readErr == nil {
		before = floodlightState(beforeMode, beforeState)
	}

	result := writeResultPage{
		Title:  cam.Name + " floodlight",
		Camera: cam,
		Before: before,
	}
	if err := setFloodlight(ctx, c, opt.Mode, opt.State); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
	} else if mode, state, readErr := readFloodlight(ctx, c); readErr != nil {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command, but reading it back failed: %v. This is not proof the camera changed anything.", readErr)
	} else {
		after := floodlightState(mode, state)
		result.After = after
		if after == opt.Value {
			result.Outcome = "accepted"
			result.Detail = "the camera reports " + after + " on a read-back over the same session that carried the write, which can answer from state the camera has not committed. This is not confirmed: see docs/control.md."
		} else {
			result.Outcome = "accepted"
			result.Detail = fmt.Sprintf("the camera answered success, but reads back as %q rather than %q. Acceptance is not effect: see message 288 in docs/control.md.", after, opt.Value)
		}
	}

	// Restore resubmits the same form with the state read before this
	// write: floodlightState's own output is exactly the option Value
	// floodlightOptionByValue accepts, so a valid Before is always a
	// resubmittable option.
	if _, ok := floodlightOptionByValue(result.Before); ok {
		result.RestoreAction = fmt.Sprintf("/cameras/%s/floodlight", cam.Name)
		result.RestoreParam = "option"
		result.RestoreValue = result.Before
	}

	s.render(w, "result.html", result)
}
