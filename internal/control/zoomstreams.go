package control

import (
	"net/http"
	"net/url"
	"slices"

	"github.com/VoltTech21/reostream/internal/config"
)

// A TrackMix (and the other two-lens trackers) has a zoom lens on channel
// 1, served as the zoom and zoom_sub streams, but only when the camera's
// streams list names them. A camera added before the scan offered them, or
// added while its zoom could not be read, had no control on its own page
// to turn them on; the only way was editing the config (YseanY, TrackMix,
// 11-02: "/driveway_zoom_sub is not served"). So the Live tab offers them
// when the camera reports a second lens and pulls neither.
//
// Nothing turns them on without being asked. zoom_sub is cheap. zoom is the
// lens at full size, and it takes one of the camera's two main stream
// slots: with main and zoom both pulled, nothing else (the Reolink app,
// another recorder) gets a main stream from that camera.

// zoomOffer reports whether the Live tab offers cam's zoom streams: the
// camera has a second lens and the config pulls neither of them.
func (s *Server) zoomOffer(cam Camera, id camIdent) bool {
	if !id.Zoom {
		return false
	}
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		return false
	}
	c, ok := cameraNamed(cfg, cam.Name)
	return ok && !slices.Contains(c.Streams, "zoom") && !slices.Contains(c.Streams, "zoom_sub")
}

// serveZoomStreams adds a camera's zoom streams to its config block and
// reloads: which=sub adds zoom_sub, which=both adds zoom and zoom_sub. A
// camera served over RTSP (its rtsp list is not empty) serves them there
// too, since that is where the recorder that asked for them reads.
func (s *Server) serveZoomStreams(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := "/cameras/" + url.PathEscape(name)
	flash := func(outcome, msg string) {
		s.setFlash(w, r, Flash{Outcome: outcome, Message: name + ": " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	var add []string
	switch r.FormValue("which") {
	case "sub":
		add = []string{"zoom_sub"}
	case "both":
		add = []string{"zoom", "zoom_sub"}
	default:
		flash("refused", "zoom streams: say which (sub or both)")
		return
	}
	text, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		flash("refused", "zoom streams: "+err.Error())
		return
	}
	// LoadRaw, as saveCamera: the block is written back, and a "$NAME"
	// password must stay a reference.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		flash("refused", "zoom streams: "+err.Error())
		return
	}
	cam, ok := cameraNamed(cfg, name)
	if !ok {
		http.Error(w, "no camera named "+name, http.StatusNotFound)
		return
	}
	served := len(cam.RTSP) > 0
	cam.Streams = slices.Clone(cam.Streams)
	cam.RTSP = slices.Clone(cam.RTSP)
	for _, st := range add {
		if !slices.Contains(cam.Streams, st) {
			cam.Streams = append(cam.Streams, st)
		}
		if served && !slices.Contains(cam.RTSP, st) {
			cam.RTSP = append(cam.RTSP, st)
		}
	}
	if _, err := s.writeAndApply(upsertCameraBlock(text, cam)); err != nil {
		flash("refused", "zoom streams: "+err.Error())
		return
	}
	if len(add) == 1 {
		flash("confirmed", "now pulling zoom_sub")
	} else {
		flash("confirmed", "now pulling zoom and zoom_sub")
	}
}
