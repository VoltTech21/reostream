// Time: the clock, and nothing else. The project this task belongs to
// started from one camera's OSD clock drifting, and keeping a fleet's
// clocks in step is the point of curating this at all.
//
// This goes over CGI, not Baichuan. The recovered message table (see
// docs/control.md, and the id/name pairs in internal/baichuan/msgids.go)
// carries no set-time message at all: nothing that writes NTP configuration
// or the clock exists in it, so there is nothing to pair a Get with here the
// way settings.go pairs "osd get" with "osd set". SetNtp and SetTime are
// real CGI commands and they are the only way this codebase has found to
// change either setting.
//
// Both forms on this page carry an "apply to every camera" checkbox,
// unticked. Ticking it runs that same single-camera write once per camera
// through applyAll in fleet.go and reports every camera's own answer, one
// row each. It replaced a separate /fleet/apply page that carried a second
// copy of these two forms, always targeted the whole fleet, and had no way
// to write one camera at all.
//
// Accounts share this file's route wiring in camctl.go but not this file:
// they get their own, accounts.go, because they are read only for a
// different and much sharper reason than "no message id was recovered".
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// readNTPDoc reads the camera's whole GetNtp document as a generic map
// rather than a fixed four-field struct.
//
// Port and Interval exist on the wire and this code offers no control for
// either: composing a SetNtp body from a struct that only knows Server and
// Enable would send zeros for every field this code has never modelled,
// exactly the mistake setField's own comment in settings.go warns against
// for XML. setNTP changes only server and enable on top of whatever this
// returns, never the whole document.
func readNTPDoc(ctx context.Context, c *cgi.Client) (map[string]any, error) {
	value, _, _, err := c.Get(ctx, "GetNtp", 0)
	if err != nil {
		return nil, err
	}
	var v struct {
		Ntp map[string]any `json:"Ntp"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, fmt.Errorf("control: parsing GetNtp: %w", err)
	}
	if v.Ntp == nil {
		return nil, fmt.Errorf("control: GetNtp carried no Ntp document")
	}
	return v.Ntp, nil
}

// ntpEnableServer reads the two fields this page actually curates off an
// NTP document, defaulting to the zero value for a field the document does
// not carry rather than failing outright: a document this code cannot
// fully parse is still worth showing what could be read from it.
func ntpEnableServer(doc map[string]any) (enabled bool, server string) {
	if f, ok := doc["enable"].(float64); ok {
		enabled = f != 0
	}
	if s, ok := doc["server"].(string); ok {
		server = s
	}
	return enabled, server
}

// deviceTimeZone is the small slice of GetTime this page actually renders:
// the camera's own timezone setting.
//
// It is the DISPLAY read that is narrow, not the write. setTimeZone reads
// the whole GetTime document back through readFullTime and changes one key
// on top of it, because composing a SetTime body out of this struct would
// send zeros for every clock field this code has never modelled. See
// readFullTime and setTimeZone below.
type deviceTimeZone struct {
	TimeZone int `json:"timeZone"`
}

type timeParam struct {
	Time deviceTimeZone `json:"Time"`
}

// ntpState is how an NTP reading is shown on the page and compared after a
// write: on/off, and the server it names either way, so a person can see
// at a glance whether the field they are about to change already matches
// what they are about to send.
func ntpState(enabled bool, server string) string {
	state := "off"
	if enabled {
		state = "on"
	}
	return fmt.Sprintf("%s, server %q", state, server)
}

// readNTP reads the camera's current enable/server pair over CGI, for
// display.
func readNTP(ctx context.Context, c *cgi.Client) (enabled bool, server string, err error) {
	doc, err := readNTPDoc(ctx, c)
	if err != nil {
		return false, "", err
	}
	enabled, server = ntpEnableServer(doc)
	return enabled, server, nil
}

// readTimeZone reads the camera's timezone over CGI.
//
// GetTime carries the camera's clock fields too (year, month, hour, and so
// on), none of which this page shows: a clock reading is stale the instant
// it is read, and rendering one invites a person to compare it against a
// wall clock and draw a conclusion this snapshot cannot support. The
// timezone is the one field on this reply that is actually a setting rather
// than a sample, so it is the one field read out here.
func readTimeZone(ctx context.Context, c *cgi.Client) (int, error) {
	value, _, _, err := c.Get(ctx, "GetTime", 0)
	if err != nil {
		return 0, err
	}
	var v timeParam
	if err := json.Unmarshal(value, &v); err != nil {
		return 0, fmt.Errorf("control: parsing GetTime: %w", err)
	}
	return v.Time.TimeZone, nil
}

// setNTP sends server and enabled to the camera over CGI SetNtp, changing
// only those two fields on top of the camera's current NTP document, and
// reports what actually happened using the same confirmed/accepted/refused
// vocabulary writeBlock uses for a Baichuan write, for the same reason
// setFloodlight in lights.go already does: SetNtp answering 200 is not
// evidence of effect any more than a Baichuan status 200 is, so this reads
// GetNtp back rather than trusting the write's own reply.
//
// It reads the camera's current NTP document first through readNTPDoc and
// changes only Server and Enable on top of it, never composing Port or
// Interval from nothing; see readNTPDoc's own comment for why.
//
// This is the only NTP write in the package: the single-camera form calls
// it directly, and the every-camera checkbox calls it once per camera
// through applyAll. Two implementations of one write would be two things
// to keep honest about read-back.
func (s *Server) setNTP(ctx context.Context, cam Camera, server string, enabled bool) (WriteResult, error) {
	c, err := s.cgiDial(cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: connecting to %q for NTP: %w", cam.Name, err)
	}

	cur, err := readNTPDoc(ctx, c)
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: reading NTP for %q before writing: %w", cam.Name, err)
	}
	curEnabled, curServer := ntpEnableServer(cur)
	before := ntpState(curEnabled, curServer)

	next := make(map[string]any, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	next["server"] = server
	if enabled {
		next["enable"] = 1
	} else {
		next["enable"] = 0
	}

	result := WriteResult{Before: []byte(before)}
	if err := c.Set(ctx, "SetNtp", map[string]any{"Ntp": next}); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
		return result, nil
	}

	afterEnabled, afterServer, readErr := readNTP(ctx, c)
	if readErr != nil {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command, but reading it back failed: %v. This is not proof the camera changed anything.", readErr)
		return result, nil
	}
	afterState := ntpState(afterEnabled, afterServer)
	result.After = []byte(afterState)
	if afterServer == server && afterEnabled == enabled {
		result.Outcome = "confirmed"
		result.Detail = "confirmed: read back after the write, and the camera reports " + afterState + "."
	} else {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera answered success, but reads back as %s rather than %s. Acceptance is not effect.", afterState, ntpState(enabled, server))
	}
	return result, nil
}

// readFullTime reads the camera's whole GetTime document as a generic map
// rather than the narrow deviceTimeZone struct this page reads for display.
//
// GetTime carries fields this codebase does not model at all: year, month,
// day, hour, minute, second, and more, most of them a sample rather than a
// setting. setTimeZone changes only one key on top of whatever this
// returns; composing a SetTime body from a struct that only knows about
// timeZone would send zeros for every field this code has never modelled,
// exactly the mistake setField's own comment in settings.go warns against
// for XML, and setNTP already avoids for Port and Interval.
func readFullTime(ctx context.Context, c *cgi.Client) (map[string]any, error) {
	value, _, _, err := c.Get(ctx, "GetTime", 0)
	if err != nil {
		return nil, err
	}
	var v struct {
		Time map[string]any `json:"Time"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, fmt.Errorf("control: parsing GetTime: %w", err)
	}
	return v.Time, nil
}

// setTimeZone sends tz to cam over CGI SetTime, changing only the timeZone
// field on top of the camera's current clock document, and reports what
// actually happened using the same confirmed/accepted/refused vocabulary
// setNTP already uses for the same reason: a 200 from SetTime is not
// evidence the camera changed anything, only a fresh read-back is.
//
// As with setNTP, this is the package's only timezone write: the
// single-camera form and the every-camera checkbox both go through it.
func (s *Server) setTimeZone(ctx context.Context, cam Camera, tz int) (WriteResult, error) {
	c, err := s.cgiDial(cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: connecting to %q for the timezone: %w", cam.Name, err)
	}

	cur, err := readFullTime(ctx, c)
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: reading the clock for %q before writing: %w", cam.Name, err)
	}
	before := fmt.Sprintf("timezone %v", cur["timeZone"])

	next := make(map[string]any, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	next["timeZone"] = tz

	result := WriteResult{Before: []byte(before)}
	if err := c.Set(ctx, "SetTime", map[string]any{"Time": next}); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
		return result, nil
	}

	after, readErr := readFullTime(ctx, c)
	if readErr != nil {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command, but reading it back failed: %v. This is not proof the camera changed anything.", readErr)
		return result, nil
	}
	afterState := fmt.Sprintf("timezone %v", after["timeZone"])
	result.After = []byte(afterState)

	matched := false
	if f, ok := after["timeZone"].(float64); ok && int(f) == tz {
		matched = true
	}
	if matched {
		result.Outcome = "confirmed"
		result.Detail = "confirmed: read back after the write, and the camera reports " + afterState + "."
	} else {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera answered success, but reads back as %s rather than timezone %d. Acceptance is not effect.", afterState, tz)
	}
	return result, nil
}

// timePage is what time.html renders.
type timePage struct {
	Title  string
	Camera Camera

	// Flash is the one-line report a write on this page left behind, or
	// nil when this page was simply opened. See flash.go.
	Flash *Flash

	NTPServer  string
	NTPEnabled bool
	NTPErr     string

	TimeZone    int
	TimeZoneErr string

	// TimeZoneLabel is TimeZone read the way a person reads a timezone,
	// and TimeZoneChoices is the picker beside the raw field. See
	// timezone.go: the field counts seconds WEST of UTC, so the number
	// and the label carry opposite signs and the bare number reads as a
	// mystery on its own.
	TimeZoneLabel   string
	TimeZoneChoices []timeZoneChoice

	// Results is one row per camera, filled in only when a write on this
	// page was ticked "apply to every camera". It is never a summary: see
	// applyAll in fleet.go for why a fleet of three models cannot honestly
	// be collapsed into one line.
	//
	// A single-camera write leaves this nil and reports through Flash
	// instead, which is the default and the ordinary case.
	Results []FleetResult

	// ResultsTarget names which setting those rows belong to ("ntp",
	// "timezone"). The two settings write through two different CGI
	// commands, and a camera refusing one must not be read as it refusing
	// the other.
	ResultsTarget string
}

// readTimePage builds the page from a fresh read of this camera: NTP and
// timezone as the camera currently holds them.
//
// serveTime and both POST handlers use it, so the forms an operator lands
// on are always seeded from the camera rather than from whatever was just
// submitted: a write that was accepted but did not take must not leave the
// page showing the value that failed to stick.
func (s *Server) readTimePage(ctx context.Context, cam Camera) timePage {
	page := timePage{Title: cam.Name + " time", Camera: cam, TimeZoneChoices: timeZoneChoices()}

	c, err := s.cgiDial(cam)
	if err != nil {
		page.NTPErr = fmt.Sprintf("could not connect: %v", err)
		page.TimeZoneErr = page.NTPErr
		return page
	}

	if enabled, server, readErr := readNTP(ctx, c); readErr != nil {
		page.NTPErr = fmt.Sprintf("could not read NTP: %v", readErr)
	} else {
		page.NTPServer = server
		page.NTPEnabled = enabled
	}

	if tz, readErr := readTimeZone(ctx, c); readErr != nil {
		page.TimeZoneErr = fmt.Sprintf("could not read the timezone: %v", readErr)
	} else {
		page.TimeZone = tz
		page.TimeZoneLabel = utcOffsetLabel(tz)
	}
	return page
}

// timeCamera resolves the camera named in the URL, answering 404 itself
// when there is no such camera. serveTime and both POST handlers open the
// same way, and a name that is not in the config is the same answer for
// all three.
func (s *Server) timeCamera(w http.ResponseWriter, r *http.Request) (Camera, bool) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return Camera{}, false
	}
	return cam, true
}

// serveTime shows the camera's current NTP configuration and timezone, both
// read fresh over CGI, seeding the forms the same way serveSettings seeds a
// curated Baichuan field: from what the camera actually holds, not a guess.
func (s *Server) serveTime(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.timeCamera(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	page := s.readTimePage(ctx, cam)
	page.Flash = s.takeFlash(r)
	s.render(w, "time.html", page)
}

// applyEveryCamera reports whether the operator ticked "apply to every
// camera" on the form they submitted.
//
// The checkbox is unticked by default, and an unticked checkbox sends
// nothing at all, so the field being absent means this camera only. That
// default is the point of the change: the page this replaced had no choice
// to make, its only button was "apply to every camera". Writing eight
// cameras is now something asked for, not what happens to anyone who fills
// in a form without reading it.
func applyEveryCamera(r *http.Request) bool {
	return r.FormValue("every") == "1"
}

// renderFleetResults re-reads this camera and renders its time page with
// one row per camera below the forms.
//
// It renders rather than redirecting the way the single-camera path does: a
// flash carries one line, and the whole point of an every-camera apply is
// that there are as many lines as there are cameras and none of them may be
// dropped on the way to the page.
func (s *Server) renderFleetResults(w http.ResponseWriter, r *http.Request, cam Camera, target string, results []FleetResult) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	page := s.readTimePage(ctx, cam)
	page.ResultsTarget = target
	page.Results = results
	s.render(w, "time.html", page)
}

// serveApplyTime is the NTP form's POST: setNTP against this camera, or,
// when the operator ticked the box, against every camera in the fleet with
// one result reported each.
func (s *Server) serveApplyTime(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.timeCamera(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	server := r.FormValue("server")
	enabled := r.FormValue("enabled") == "1"

	if applyEveryCamera(r) {
		// The request context goes through unclamped: applyAll gives each
		// camera its own fleetApplyTimeout, and wrapping the whole fleet
		// in one probeTimeout here would take that budget back.
		results := s.applyAll(r.Context(), func(ctx context.Context, c Camera) (WriteResult, error) {
			return s.setNTP(ctx, c, server, enabled)
		})
		s.renderFleetResults(w, r, cam, "ntp", results)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	result, err := s.setNTP(ctx, cam, server, enabled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Back to this camera's own time page with a one-line report, rather
	// than onto result.html: see serveApplySetting for why. There is no
	// undo offered here. Before is a rendering of the previous state
	// ("on, server \"pool.ntp.org\""), not a resubmittable pair of form
	// fields, and a button that posted that string back as a server name
	// would write nonsense to the camera. The previous server is still on
	// the page the operator lands on, in the form they just used.
	s.setFlash(w, r, flashFor(cam.Name+": ntp", result))
	http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name)+"/time", http.StatusSeeOther)
}

// serveApplyTimezone is the timezone form's POST: setTimeZone against this
// camera, or against every camera when the box is ticked.
//
// It is its own route rather than a mode of the NTP one because the two
// settings reach the camera through two different CGI commands, so a camera
// that refuses one may well take the other, and one POST carrying both
// would have to report that as a single outcome.
func (s *Server) serveApplyTimezone(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.timeCamera(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tz, err := strconv.Atoi(strings.TrimSpace(r.FormValue("timezone")))
	if err != nil {
		http.Error(w, "timezone must be a number", http.StatusBadRequest)
		return
	}

	if applyEveryCamera(r) {
		results := s.applyAll(r.Context(), func(ctx context.Context, c Camera) (WriteResult, error) {
			return s.setTimeZone(ctx, c, tz)
		})
		s.renderFleetResults(w, r, cam, "timezone", results)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	result, err := s.setTimeZone(ctx, cam, tz)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// No undo here either, and for the same reason: Before is a rendering
	// ("timezone -28800"), not a resubmittable field, and the previous
	// value is back in the form on the page the operator lands on.
	s.setFlash(w, r, flashFor(cam.Name+": timezone", result))
	http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name)+"/time", http.StatusSeeOther)
}
