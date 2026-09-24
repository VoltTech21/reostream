package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// The fisheye's view modes.
//
// A fisheye sensor sees a circle. The camera can hand that over raw, or
// dewarp it into a panorama, a 2x2 grid of corrected views, or two stacked
// halves, and it does the dewarping itself: a client cropping regions out
// of the circle and correcting them is redoing work the camera does
// properly. All four come out at the same frame size.
//
// This is the most expensive write on the page, and the page has to say so
// before anyone presses it:
//
//   - the camera REBOOTS. Its HTTP API answers 502 or nothing for fifteen
//     to twenty seconds, the auth token is invalidated, and every recorder
//     reading it loses the stream and reconnects.
//   - every motion mask and detection zone drawn against the old geometry
//     is now pointing somewhere else. Those coordinates are normalised to
//     the frame, so a zone drawn on the circle is not the same place once
//     the picture is a panorama.
//
// Which is why this is a form with its own confirmation and its own
// warning, rather than another row in a settings group.

// fisheyeOption is one view mode as the page offers it.
type fisheyeOption struct {
	Mode  int
	Value string
	What  string
}

// fisheyeOptions are the four modes, in the camera's own numbering.
var fisheyeOptions = []fisheyeOption{
	{Mode: 0, Value: "circle", What: "the raw fisheye circle, uncorrected"},
	{Mode: 1, Value: "panorama", What: "the circle unwrapped into a flat band"},
	{Mode: 2, Value: "quad", What: "four dewarped views in a 2x2 grid"},
	{Mode: 3, Value: "dual", What: "two dewarped halves, stacked"},
}

func fisheyeOptionByValue(v string) (fisheyeOption, bool) {
	for _, o := range fisheyeOptions {
		if o.Value == v {
			return o, true
		}
	}
	return fisheyeOption{}, false
}

func fisheyeValueByMode(mode int) string {
	for _, o := range fisheyeOptions {
		if o.Mode == mode {
			return o.Value
		}
	}
	return fmt.Sprintf("mode %d", mode)
}

// fisheyeConfirmReason is what the operator is asked before the write. It
// names both consequences, because neither is recoverable by pressing the
// button again.
const fisheyeConfirmReason = "This reboots the camera. It will be offline for about twenty seconds, every recorder reading it will reconnect, and every motion mask and detection zone on this camera will be pointing at the wrong place afterwards, because those coordinates are relative to the picture. Change the view?"

// readFisheye reads the camera's current view mode.
func readFisheye(ctx context.Context, c *cgi.Client) (cgi.FishEye, error) {
	return c.GetFishEye(ctx, 0)
}

func (s *Server) serveApplyFisheye(w http.ResponseWriter, r *http.Request) {
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
	opt, ok := fisheyeOptionByValue(r.FormValue("option"))
	if !ok {
		http.Error(w, fmt.Sprintf("%q is not a fisheye view", r.FormValue("option")), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	c, err := s.cgiDial(cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("connecting to %s: %v", cam.Name, err), http.StatusBadGateway)
		return
	}

	// The current view, for the undo and for Before. Best effort: a failed
	// pre-read is not a reason to refuse the write.
	var before string
	current, readErr := readFisheye(ctx, c)
	if readErr == nil {
		before = fisheyeValueByMode(int(current.ImageType))
	}
	if before == opt.Value {
		s.setFlash(w, r, flashFor(cam.Name+": fisheye view", WriteResult{
			Outcome: "accepted",
			Before:  []byte(before),
			Detail:  "the camera is already in that view, so nothing was written and it was not rebooted.",
		}))
		http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name), http.StatusSeeOther)
		return
	}

	next := current
	next.ImageType = cgi.FishEyeMode(opt.Mode)
	result := WriteResult{Before: []byte(before)}
	if err := c.SetFishEye(ctx, 0, next); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
	} else {
		// No read-back, deliberately. The camera is rebooting: a read now
		// answers 502, or worse answers from the session that is about to
		// die, and either would be reported as though it said something
		// about the new view. Saying plainly that it is rebooting is more
		// honest than a read-back that cannot mean anything yet.
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf("the camera took the change to %s and is rebooting. It will be back in about twenty seconds; this page will not know the new view until then. Motion masks and zones on this camera now need redrawing.", opt.Value)
	}

	flash := flashFor(cam.Name+": fisheye view", result)
	// An undo is offered, but it is another reboot, not a cheap revert.
	// The flash says so in Detail above; the button is here because
	// changing the view by accident is worse than changing it back.
	if _, ok := fisheyeOptionByValue(before); ok && result.Outcome != "refused" {
		flash.UndoAction = fmt.Sprintf("/cameras/%s/fisheye", cam.Name)
		flash.UndoParam = "option"
		flash.UndoValue = before
	}
	s.setFlash(w, r, flash)
	http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name), http.StatusSeeOther)
}

// fisheyeSettleDelay is how long a camera takes to come back from the
// reboot a view change causes. Nothing waits on it: it is here so the
// number the page quotes and the number in the docs are the same one.
const fisheyeSettleDelay = 20 * time.Second
