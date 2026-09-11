// Fleet apply: push one curated clock setting to every configured camera
// in one request, and report every camera's own outcome.
//
// The problem this exists for is the OSD clock on one camera drifting out
// of step with the other seven. Fixing that by hand, one camera page at a
// time, is exactly the chore this feature removes: this package can already
// set NTP and read the timezone on a single camera (time.go), fleet apply
// is the same write, run once per camera, with its own result reported for
// each.
//
// Eight cameras are eight independent outcomes. Of the 103 messages probed
// across the three models this fleet actually has, 14 differ, and the
// fisheye and the dual lens model account for most of that disagreement.
// So a fleet apply that confirms on six cameras and refuses on two is not a
// failure to collapse into one line, it is the ordinary shape of the
// answer, and applyAll's whole job is to keep that shape intact all the way
// to the page.
package camctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// FleetResult is what happened when a fleet apply reached one camera.
//
// Outcome carries the same confirmed/accepted/refused vocabulary
// WriteResult uses everywhere else in this package: a fleet apply is not a
// different kind of write, it is the same write attempted once per camera,
// and it must not grow a second vocabulary to describe the same three
// things.
type FleetResult struct {
	Camera  string
	Outcome string
	Detail  string
}

// fleetApplyTimeout bounds one camera's turn in a fleet apply.
//
// It is given fresh to every camera applyAll tries, never shared off one
// overall deadline: a camera that is unreachable, powered off, or mid
// reboot must be free to burn its own timeout without leaving less time for
// the cameras behind it in the list.
//
// This is deliberately its own number, not probeTimeout. probeTimeout
// bounds a sweep of around a hundred reads with redials; a fleet apply does
// one CGI round trip per camera (setNTP and setTimeZone each run at most a
// dial, a read, a write, and a read-back: up to four calls, each bounded by
// cgi.Client's own 20 second http.Client timeout, so at most about 80
// seconds in the worst case). applyAll runs the fleet serially, so this
// number is multiplied by the fleet size on the bad day every camera is
// down: at 3 minutes, an eight-camera fleet that is entirely unreachable
// would leave the page looking hung for 24 minutes. 90 seconds covers the
// worst-case four-call chain with headroom without multiplying into
// something an operator would give up on and kill.
//
// This is a ceiling, not a guarantee that a hung CGI call gets cancelled
// mid-flight: cgi.Client.Get and Set take no context, so what this actually
// bounds is the gap between calls, the same property setNTP already has.
const fleetApplyTimeout = 90 * time.Second

// applyAll runs apply against every configured camera and returns one
// result per camera, in fleet order. It never stops at the first failure,
// and it never skips a camera because an earlier one failed: a fleet apply
// that stopped at the first dead camera would not be a fleet tool, it would
// be a loop with delusions of one.
//
// Each camera gets its own context, derived from ctx but carrying its own
// fresh fleetApplyTimeout, so one camera's bound cannot be eaten by another
// camera's turn ahead of it in the list.
func (s *Server) applyAll(ctx context.Context, apply func(context.Context, Camera) (WriteResult, error)) []FleetResult {
	cams, err := s.fleet()
	if err != nil {
		return []FleetResult{{Outcome: "refused", Detail: fmt.Sprintf("could not read the fleet: %v", err)}}
	}

	results := make([]FleetResult, 0, len(cams))
	for _, cam := range cams {
		camCtx, cancel := context.WithTimeout(ctx, fleetApplyTimeout)
		result, err := apply(camCtx, cam)
		cancel()
		if err != nil {
			results = append(results, FleetResult{Camera: cam.Name, Outcome: "refused", Detail: err.Error()})
			continue
		}
		results = append(results, FleetResult{Camera: cam.Name, Outcome: result.Outcome, Detail: result.Detail})
	}
	return results
}

// fleetApplyTargets is the whole list of settings fleet apply may push to
// every camera at once, keyed by the id its own route and form use.
//
// This is how a setting becomes fleet-appliable: by being added here,
// explicitly, by name. Not by being editable. settings.go's groups() and
// time.go's own NTP form both already expose more editable fields than
// this map lists, and none of them gained the ability to be pushed to
// eight cameras in one request just by having a form control; only NTP
// server and timezone have earned a row here, and the next field that
// wants one needs its own line added on purpose.
var fleetApplyTargets = map[string]string{
	"ntp":      "NTP server",
	"timezone": "Timezone",
}

// fleetApplyPage is what fleetapply.html renders: the form, or, once a
// target has been submitted, the per-camera table applyAll produced.
type fleetApplyPage struct {
	Title string
	// Target is the id of the setting a results table below belongs to,
	// or empty when the page is showing only the two blank forms.
	Target  string
	Results []FleetResult
	Err     string
}

// serveFleetApplyForm shows the two fleet-appliable forms with no results
// yet: NTP server plus enabled, and timezone, the only two entries in
// fleetApplyTargets.
func (s *Server) serveFleetApplyForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "fleetapply.html", fleetApplyPage{Title: "Fleet apply"})
}

// serveFleetApplyNTP pushes one NTP server and enabled flag to every camera
// in the fleet, through the same setNTP a single camera's time page already
// uses, and renders one row per camera.
func (s *Server) serveFleetApplyNTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	server := r.FormValue("server")
	enabled := r.FormValue("enabled") == "1"

	apply := func(ctx context.Context, cam Camera) (WriteResult, error) {
		return s.setNTP(ctx, cam, server, enabled)
	}
	results := s.applyAll(r.Context(), apply)
	s.render(w, "fleetapply.html", fleetApplyPage{
		Title:   "Fleet apply: NTP server",
		Target:  "ntp",
		Results: results,
	})
}

// serveFleetApplyTimezone pushes one timezone value to every camera in the
// fleet, through setTimeZone below, and renders one row per camera.
func (s *Server) serveFleetApplyTimezone(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tz, err := strconv.Atoi(r.FormValue("timezone"))
	if err != nil {
		http.Error(w, "timezone must be a number", http.StatusBadRequest)
		return
	}

	apply := func(ctx context.Context, cam Camera) (WriteResult, error) {
		return s.setTimeZone(ctx, cam, tz)
	}
	results := s.applyAll(r.Context(), apply)
	s.render(w, "fleetapply.html", fleetApplyPage{
		Title:   "Fleet apply: timezone",
		Target:  "timezone",
		Results: results,
	})
}

// readFullTime reads the camera's whole GetTime document as a generic map
// rather than the narrow deviceTime struct time.go reads for display.
//
// GetTime carries fields this codebase does not model at all: year, month,
// day, hour, minute, second, and more, most of them a sample rather than a
// setting. setTimeZone changes only one key on top of whatever this
// returns; composing a SetTime body from a struct that only knows about
// timeZone would send zeros for every field this code has never modelled,
// exactly the mistake setField's own comment in settings.go warns against
// for XML, and setNTP already avoids for Port and Interval.
func readFullTime(c *cgi.Client) (map[string]any, error) {
	value, _, _, err := c.Get("GetTime", 0)
	if err != nil {
		return nil, err
	}
	var v struct {
		Time map[string]any `json:"Time"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, fmt.Errorf("camctl: parsing GetTime: %w", err)
	}
	return v.Time, nil
}

// setTimeZone sends tz to cam over CGI SetTime, changing only the timeZone
// field on top of the camera's current clock document, and reports what
// actually happened using the same confirmed/accepted/refused vocabulary
// setNTP already uses for the same reason: a 200 from SetTime is not
// evidence the camera changed anything, only a fresh read-back is.
func (s *Server) setTimeZone(ctx context.Context, cam Camera, tz int) (WriteResult, error) {
	c, err := s.cgiDial(cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: connecting to %q for the timezone: %w", cam.Name, err)
	}

	cur, err := readFullTime(c)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: reading the clock for %q before writing: %w", cam.Name, err)
	}
	before := fmt.Sprintf("timezone %v", cur["timeZone"])

	next := make(map[string]any, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	next["timeZone"] = tz

	result := WriteResult{Before: []byte(before)}
	if err := c.Set("SetTime", map[string]any{"Time": next}); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
		return result, nil
	}

	after, readErr := readFullTime(c)
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
