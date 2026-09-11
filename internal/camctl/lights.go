// Lights and IR: the curated controls whose write is not a document field
// but a physical effect a person or a camera can see happen. That is what
// separates this file from settings.go's own groups: every control here
// confirms before acting, every time, not once per session, because a light
// switching on in a shop at two in the morning should never be a stray
// click.
package camctl

import (
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

// whiteLed is the shape GetWhiteLed and SetWhiteLed carry over CGI.
//
// mode and state are different things, and conflating them makes the light
// look as though it only has an on switch. mode is what the light does;
// state is whether it is lit right now. See docs/control.md, "The
// floodlight, as a worked example": mode 1 with state 1 is "on", mode 1
// with state 0 is "motion", which is how these cameras ship.
type whiteLed struct {
	Channel int `json:"channel"`
	Mode    int `json:"mode"`
	State   int `json:"state"`
}

// whiteLedParam wraps whiteLed the way SetWhiteLed's own param needs it:
// the camera's CGI commands take their argument under a key matching the
// command's own object name, confirmed by GetWhiteLed's reply carrying the
// same "WhiteLed" wrapper.
type whiteLedParam struct {
	WhiteLed whiteLed `json:"WhiteLed"`
}

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
func (s *Server) cgiDial(cam Camera) (*cgi.Client, error) {
	if s.opts.CGIDial != nil {
		return s.opts.CGIDial(cam)
	}
	return cgi.Dial(cam.Address, cam.Username, cam.Password)
}

// readFloodlight reads the floodlight's current mode and state over CGI.
//
// GetWhiteLed intermittently answers "please login first" on a connection
// that logged in seconds earlier; c already logs in again and retries once,
// so nothing here adds a second retry layer on top of it.
func readFloodlight(c *cgi.Client) (mode, state int, err error) {
	value, _, _, err := c.Get("GetWhiteLed", 0)
	if err != nil {
		return 0, 0, err
	}
	var v struct {
		WhiteLed whiteLed `json:"WhiteLed"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return 0, 0, fmt.Errorf("camctl: parsing GetWhiteLed: %w", err)
	}
	return v.WhiteLed.Mode, v.WhiteLed.State, nil
}

// setFloodlight sends mode and state over CGI.
//
// This is the floodlight's only working write path. Its Baichuan
// equivalent, message 288, is known inert on this firmware: it takes a
// document built with the correct element name, taken from the firmware
// rather than guessed, and answers 200 while changing nothing observable. A
// wrong element name on the same message answers 400, so the document is
// parsed and then discarded, not merely unsupported. Routing the floodlight
// through writeBlock because it looks tidier next to the rest of this
// codebase would not work.
func setFloodlight(c *cgi.Client, mode, state int) error {
	return c.Set("SetWhiteLed", whiteLedParam{WhiteLed: whiteLed{Channel: 0, Mode: mode, State: state}})
}

// serveApplyFloodlight is the floodlight's POST: set the requested option
// over CGI, then read GetWhiteLed back on the same session to see whether
// it actually took, the identical discipline writeBlock applies to a
// Baichuan write, using the same three-word vocabulary so an operator reads
// one meaning for "confirmed" everywhere on this page.
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

	c, err := s.cgiDial(cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("connecting to %s: %v", cam.Name, err), http.StatusBadGateway)
		return
	}

	// Best effort: a failed pre-read is not a reason to refuse the write
	// itself, only to leave Before blank.
	var before string
	if beforeMode, beforeState, readErr := readFloodlight(c); readErr == nil {
		before = floodlightState(beforeMode, beforeState)
	}

	result := writeResponse{Before: before}
	if err := setFloodlight(c, opt.Mode, opt.State); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
	} else if mode, state, readErr := readFloodlight(c); readErr != nil {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command, but reading it back failed: %v. This is not proof the camera changed anything.", readErr)
	} else {
		after := floodlightState(mode, state)
		result.After = after
		if after == opt.Value {
			result.Outcome = "confirmed"
			result.Detail = "confirmed: read back after the write, and the light reports " + after + "."
		} else {
			result.Outcome = "accepted"
			result.Detail = fmt.Sprintf("the camera answered success, but reads back as %q rather than %q. Acceptance is not effect: see message 288 in docs/control.md.", after, opt.Value)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
