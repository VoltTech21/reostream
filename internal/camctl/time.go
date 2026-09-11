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

// ntp is the shape GetNtp and SetNtp carry over CGI, under the "Ntp" key
// both the read and the write use.
//
// Port and Interval exist on the wire and this code does not offer a
// control for either. Sending SetNtp with them zeroed would be composing a
// value nobody asked for and the camera never reported holding, the same
// mistake setField's own comment warns against for XML; readNTP fills them
// from the camera's own current document first, and setNTP changes only
// Server and Enable on top of that, exactly as setField changes only the
// one field named for a raw block.
type ntp struct {
	Enable   int    `json:"enable"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Interval int    `json:"interval"`
}

// ntpParam wraps ntp the way SetNtp's own param needs it, and the way
// GetNtp's reply carries it back: under an "Ntp" key, confirmed by the
// live floodlight's identical WhiteLed wrapper in lights.go.
type ntpParam struct {
	Ntp ntp `json:"Ntp"`
}

// deviceTime is the small slice of GetTime this page actually renders: the
// camera's own timezone setting. Nothing here writes it back; see
// accountsReadOnlyWarning's sibling comment on serveTime for why this page
// only displays it.
type deviceTime struct {
	TimeZone int `json:"timeZone"`
}

type timeParam struct {
	Time deviceTime `json:"Time"`
}

// ntpState is how an NTP reading is shown on the page and compared after a
// write: on/off, and the server it names either way, so a person can see
// at a glance whether the field they are about to change already matches
// what they are about to send.
func ntpState(n ntp) string {
	state := "off"
	if n.Enable != 0 {
		state = "on"
	}
	return fmt.Sprintf("%s, server %q", state, n.Server)
}

// readNTP reads the camera's current NTP configuration over CGI.
func readNTP(c *cgi.Client) (ntp, error) {
	value, _, _, err := c.Get("GetNtp", 0)
	if err != nil {
		return ntp{}, err
	}
	var v ntpParam
	if err := json.Unmarshal(value, &v); err != nil {
		return ntp{}, fmt.Errorf("camctl: parsing GetNtp: %w", err)
	}
	return v.Ntp, nil
}

// readTimeZone reads the camera's timezone over CGI.
//
// GetTime carries the camera's clock fields too (year, month, hour, and so
// on), none of which this page shows: a clock reading is stale the instant
// it is read, and rendering one invites a person to compare it against a
// wall clock and draw a conclusion this snapshot cannot support. The
// timezone is the one field on this reply that is actually a setting rather
// than a sample, so it is the one field read out here.
func readTimeZone(c *cgi.Client) (int, error) {
	value, _, _, err := c.Get("GetTime", 0)
	if err != nil {
		return 0, err
	}
	var v timeParam
	if err := json.Unmarshal(value, &v); err != nil {
		return 0, fmt.Errorf("camctl: parsing GetTime: %w", err)
	}
	return v.Time.TimeZone, nil
}

// setNTP sends server and enabled to the camera over CGI SetNtp, and
// reports what actually happened using the same confirmed/accepted/refused
// vocabulary writeBlock uses for a Baichuan write, for the same reason
// setFloodlight in lights.go already does: SetNtp answering 200 is not
// evidence of effect any more than a Baichuan status 200 is, so this reads
// GetNtp back rather than trusting the write's own reply.
//
// It reads the camera's current NTP document first and changes only Server
// and Enable on top of it, never composing Port or Interval from nothing;
// see the ntp type's own comment for why.
func (s *Server) setNTP(ctx context.Context, cam Camera, server string, enabled bool) (WriteResult, error) {
	c, err := s.cgiDial(cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: connecting to %q for NTP: %w", cam.Name, err)
	}

	cur, err := readNTP(c)
	if err != nil {
		return WriteResult{}, fmt.Errorf("camctl: reading NTP for %q before writing: %w", cam.Name, err)
	}
	before := ntpState(cur)

	next := cur
	next.Server = server
	if enabled {
		next.Enable = 1
	} else {
		next.Enable = 0
	}

	result := WriteResult{Before: []byte(before)}
	if err := c.Set("SetNtp", ntpParam{Ntp: next}); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
		return result, nil
	}

	after, readErr := readNTP(c)
	if readErr != nil {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the command, but reading it back failed: %v. This is not proof the camera changed anything.", readErr)
		return result, nil
	}
	afterState := ntpState(after)
	result.After = []byte(afterState)
	if after.Server == next.Server && after.Enable == next.Enable {
		result.Outcome = "confirmed"
		result.Detail = "confirmed: read back after the write, and the camera reports " + afterState + "."
	} else {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera answered success, but reads back as %s rather than %s. Acceptance is not effect.", afterState, ntpState(next))
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

	c, err := s.cgiDial(cam)
	if err != nil {
		page.NTPErr = fmt.Sprintf("could not connect: %v", err)
		page.TimeZoneErr = page.NTPErr
		s.render(w, "time.html", page)
		return
	}

	if cur, readErr := readNTP(c); readErr != nil {
		page.NTPErr = fmt.Sprintf("could not read NTP: %v", readErr)
	} else {
		page.NTPServer = cur.Server
		page.NTPEnabled = cur.Enable != 0
	}

	if tz, readErr := readTimeZone(c); readErr != nil {
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(writeResponse{
		Outcome: result.Outcome,
		Detail:  result.Detail,
		Before:  string(result.Before),
		After:   string(result.After),
	})
}
