// Time: the clock, and nothing else. The project this task belongs to
// started from one camera's OSD clock drifting, and keeping a fleet's
// clocks in step is the point of curating this at all.
//
// This goes over CGI, not Baichuan. The recovered message table (see
// docs/control.md, and the id/name pairs in internal/baichuan/msgids.go)
// carries no set-time message at all: nothing that writes NTP configuration
// or the clock exists in it, so there is nothing to pair a Get with here the
// way settings.go pairs "osd get" with "osd set". SetNtp is a real CGI
// command and it is the only way this codebase has found to change either.
//
// Accounts share this file's route wiring in camctl.go but not this file:
// they get their own, accounts.go, because they are read only for a
// different and much sharper reason than "no message id was recovered".
package camctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
		return nil, fmt.Errorf("camctl: parsing GetNtp: %w", err)
	}
	if v.Ntp == nil {
		return nil, fmt.Errorf("camctl: GetNtp carried no Ntp document")
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
// the camera's own timezone setting. Nothing here writes it back; see
// accountsReadOnlyWarning's sibling comment on serveTime for why this page
// only displays it.
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
		return 0, fmt.Errorf("camctl: parsing GetTime: %w", err)
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
func (s *Server) setNTP(ctx context.Context, cam Camera, server string, enabled bool) (WriteResult, error) {
	c, err := s.cgiDial(cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: connecting to %q for NTP: %w", cam.Name, err)
	}

	cur, err := readNTPDoc(ctx, c)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: reading NTP for %q before writing: %w", cam.Name, err)
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

// timePage is what time.html renders.
type timePage struct {
	Title  string
	Camera Camera

	NTPServer  string
	NTPEnabled bool
	NTPErr     string

	TimeZone    int
	TimeZoneErr string
}

// serveTime shows the camera's current NTP configuration and timezone, both
// read fresh over CGI, seeding the NTP form the same way serveSettings
// seeds a curated Baichuan field: from what the camera actually holds, not
// a guess.
func (s *Server) serveTime(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := timePage{Title: cam.Name + " time", Camera: cam}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	c, err := s.cgiDial(cam)
	if err != nil {
		page.NTPErr = fmt.Sprintf("could not connect: %v", err)
		page.TimeZoneErr = page.NTPErr
		s.render(w, "time.html", page)
		return
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
	}

	s.render(w, "time.html", page)
}

// serveApplyTime is the NTP form's POST: setNTP, reported the same way
// every other write on this page is reported.
func (s *Server) serveApplyTime(w http.ResponseWriter, r *http.Request) {
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
	server := r.FormValue("server")
	enabled := r.FormValue("enabled") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	result, err := s.setNTP(ctx, cam, server, enabled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.render(w, "result.html", writeResultPage{
		Title:   cam.Name + " NTP",
		Camera:  cam,
		Outcome: result.Outcome,
		Detail:  result.Detail,
		Before:  string(result.Before),
		After:   string(result.After),
	})
}
