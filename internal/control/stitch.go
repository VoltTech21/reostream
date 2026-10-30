package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// The seam between two lenses.
//
// A camera with two lenses a few centimeters apart sees an object in the
// overlap at a different place in each image, by an amount that depends on
// how far away it is. Near things shift a lot, distant things barely. So no
// single alignment is right for a whole scene: the seam can be made to
// vanish at ONE depth, and `distance` picks which. Nearer than that,
// objects duplicate across the seam; further, they are clipped out of it.
//
// x and y are a fixed nudge of one image against the other, correcting how
// the lens modules are physically mounted. That error does not change with
// what the camera is looking at, which is why it is a constant and distance
// is not.
//
// Which gives the operator a rule worth putting on the page: a seam wrong
// the same way everywhere is x and y; a seam that lines up at one depth and
// splits at another is distance, and no value fixes every depth at once.
//
// Unlike the fisheye's Image Layout this does not reboot the camera and
// does not change the frame size, so it needs no confirmation.

// stitchPage is the stitch form's state: what the camera holds, what it
// shipped with, and what it will accept.
type stitchPage struct {
	Distance    float64
	X           int
	Y           int
	FacDistance float64
	FacX        int
	FacY        int
	MinDistance float64
	MaxDistance float64
	MinX        int
	MaxX        int
	MinY        int
	MaxY        int
}

// readStitch fills the form from the camera, including the range, so the
// inputs constrain themselves to what this camera actually accepts rather
// than to numbers written down here.
func readStitch(ctx context.Context, c *cgi.Client, channel int) (stitchPage, error) {
	cur, fac, lim, err := c.GetStitch(ctx, channel)
	if err != nil {
		return stitchPage{}, err
	}
	return stitchPage{
		Distance:    cur.Distance,
		X:           cur.XMove,
		Y:           cur.YMove,
		FacDistance: fac.Distance,
		FacX:        fac.XMove,
		FacY:        fac.YMove,
		MinDistance: lim.Distance.Min,
		MaxDistance: lim.Distance.Max,
		MinX:        lim.XMove.Min,
		MaxX:        lim.XMove.Max,
		MinY:        lim.YMove.Min,
		MaxY:        lim.YMove.Max,
	}, nil
}

func (s *Server) serveApplyStitch(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	c, err := s.cgiDial(cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("connecting to %s: %v", cam.Name, err), http.StatusBadGateway)
		return
	}
	defer c.Close()

	// Read first. The camera takes all three fields together, so a form
	// that posts one would otherwise zero the other two.
	cur, _, lim, err := c.GetStitch(ctx, cam.Channel)
	if err != nil {
		s.setFlash(w, r, flashFor(cam.Name+": lens alignment", WriteResult{
			Outcome: "refused",
			Detail:  fmt.Sprintf("could not read the current alignment: %v", err),
		}))
		http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name)+"/settings/stitch", http.StatusSeeOther)
		return
	}

	before := fmt.Sprintf("distance %g, x %d, y %d", cur.Distance, cur.XMove, cur.YMove)
	want := cur
	refuse := func(detail string) {
		s.setFlash(w, r, flashFor(cam.Name+": lens alignment", WriteResult{
			Outcome: "refused",
			Before:  []byte(before),
			Detail:  detail,
		}))
		http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name)+"/settings/stitch", http.StatusSeeOther)
	}

	if v := r.FormValue("distance"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			refuse(fmt.Sprintf("%q is not a distance", v))
			return
		}
		want.Distance = f
	}
	if v := r.FormValue("x"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			refuse(fmt.Sprintf("%q is not a whole number", v))
			return
		}
		want.XMove = n
	}
	if v := r.FormValue("y"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			refuse(fmt.Sprintf("%q is not a whole number", v))
			return
		}
		want.YMove = n
	}

	// Checked against the camera's own range, not a constant written here.
	// A camera that silently clamps leaves the page showing a number it is
	// not using, so an out-of-range value is refused with the bound named.
	if err := checkStitchRange(want, lim); err != nil {
		refuse(err.Error())
		return
	}

	result := WriteResult{Before: []byte(before)}
	if err := c.SetStitch(ctx, cam.Channel, want); err != nil {
		result.Outcome = "refused"
		result.Detail = err.Error()
	} else {
		result.Outcome = "accepted"
		result.Detail = fmt.Sprintf(
			"distance %g, x %d, y %d. The seam moves on the live picture; unlike the fisheye's Image Layout this does not reboot the camera or change the frame size.",
			want.Distance, want.XMove, want.YMove)
	}

	flash := flashFor(cam.Name+": lens alignment", result)
	// Cheap to undo, unlike the fisheye view: this is three numbers and no
	// reboot, so the undo really does put it back.
	if result.Outcome == "accepted" {
		flash.UndoAction = fmt.Sprintf("/cameras/%s/stitch", cam.Name)
		flash.UndoParam = "distance"
		flash.UndoValue = strconv.FormatFloat(cur.Distance, 'g', -1, 64)
	}
	s.setFlash(w, r, flash)
	http.Redirect(w, r, "/cameras/"+url.PathEscape(cam.Name)+"/settings/stitch", http.StatusSeeOther)
}

// checkStitchRange refuses a value the camera would not accept, naming the
// bound rather than reporting a generic failure.
func checkStitchRange(s cgi.Stitch, lim cgi.StitchLimits) error {
	if lim.Distance.Max > lim.Distance.Min &&
		(s.Distance < lim.Distance.Min || s.Distance > lim.Distance.Max) {
		return fmt.Errorf("distance %g is outside what this camera accepts (%g to %g)",
			s.Distance, lim.Distance.Min, lim.Distance.Max)
	}
	if lim.XMove.Max > lim.XMove.Min && (s.XMove < lim.XMove.Min || s.XMove > lim.XMove.Max) {
		return fmt.Errorf("x %d is outside what this camera accepts (%d to %d)",
			s.XMove, lim.XMove.Min, lim.XMove.Max)
	}
	if lim.YMove.Max > lim.YMove.Min && (s.YMove < lim.YMove.Min || s.YMove > lim.YMove.Max) {
		return fmt.Errorf("y %d is outside what this camera accepts (%d to %d)",
			s.YMove, lim.YMove.Min, lim.YMove.Max)
	}
	return nil
}

// alignPage is the full screen aligner's model. It carries the same numbers
// the camera page shows plus the stream to judge them against, because the
// whole point of this page is that the two are in front of you at once.
type alignPage struct {
	Title  string
	Camera string
	URL    string
	Stitch stitchPage
	// Err is why the alignment could not be read. It has to be carried
	// rather than ignored: a zero stitchPage is three valid-looking zeros
	// with a zero range, and the controls on this page write on release,
	// so rendering them over a failed read would let a nudge send
	// distance=0 x=0 y=0 to a camera whose real alignment was never known.
	Err string
	// Have says the three values came from the camera. The template shows
	// the controls only when it is true.
	Have bool
}

func (s *Server) serveAlign(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := alignPage{Title: cam.Name + " alignment", Camera: cam.Name}
	if path, ok := dewarpStream(cam.Name, s.codecsByCamera()[cam.Name]); ok {
		page.URL = tileURL(s.opts.StreamBase, path)
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	c, err := s.cgiDial(cam)
	if err != nil {
		page.Err = fmt.Sprintf("could not reach the camera: %v", err)
	} else {
		defer c.Close()
		if st, err := readStitch(ctx, c, cam.Channel); err != nil {
			page.Err = fmt.Sprintf("could not read the lens alignment: %v", err)
		} else {
			page.Stitch = st
			page.Have = true
		}
	}
	s.render(w, r, "align.html", page)
}
