package control

import (
	"net/http"
)

// The dewarped view.
//
// A fisheye sensor sees a circle, and this camera sends that circle and
// nothing else. It is worth being exact about why, because the code used to
// assume the opposite: the camera's own view-mode setting (fisheyeCfg's
// imageType, see fisheye.go) changes what it ENCODES, but the view modes an
// operator actually reaches for -- the draggable quad, the defished band --
// are drawn by whatever is displaying the picture. Measured on a live
// camera: while the phone app showed a wide defished panorama, the camera
// reported imageType 0 and a raw 2560x2560 circle, and switching the app's
// display mode wrote nothing back. A diff of all 44 readable config blocks
// across that change moved the clock and the CPU gauge and not one other
// byte.
//
// So this page asks the camera for nothing. It plays the same stream the
// dashboard tiles play and dewarps it in the browser, which costs the
// camera nothing, needs no reboot, and can show several views at once --
// none of which is true of the camera-side setting.

// viewPage is the dewarp page's model.
type viewPage struct {
	Title    string
	Camera   string
	URL      string
	Playable bool
}

func (s *Server) serveFisheyeView(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := viewPage{Title: cam.Name + " view", Camera: cam.Name}
	if path, ok := dewarpStream(cam.Name, s.codecsByCamera()[cam.Name]); ok {
		page.Playable = true
		page.URL = tileURL(s.opts.StreamBase, path)
	}
	s.render(w, r, "view.html", page)
}

// dewarpStream chooses which stream this page plays.
//
// It deliberately does not reuse playableStream, whose order is sub then
// extern. That order is right for the dashboard, where every tile plays at
// once and the sub stream is what makes that affordable. It is wrong here
// for the opposite reason: this page shows one camera, and dewarping is a
// crop -- a pane showing a quarter of the circle is sampling a quarter of
// the source's pixels, so a 1024x1024 sub stream becomes roughly a 512x512
// pane. Main on this camera is 2560x2560 H.264, which is both playable and
// four times the detail, so main comes first here and sub is the fallback.
// The URL shapes are the streaming listener's published ones, and main is
// the odd one out: it is a BARE camera name, not a "_main" suffix, because
// that is the form every recorder config written against this project
// already contains. See server.lookup, which is the code that decides.
func dewarpStream(camera string, streams map[string]string) (string, bool) {
	for _, s := range []struct{ stream, path string }{
		{"main", "/" + camera + ".ts"},
		{"extern", "/" + camera + "_extern.ts"},
		{"sub", "/" + camera + "_sub.ts"},
	} {
		if browserPlayable(streams[s.stream]) {
			return s.path, true
		}
	}
	return "", false
}
