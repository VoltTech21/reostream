package camctl

import "net/http"

// serveFleet lists the configured cameras. It is the page later tasks hang
// per-camera views and settings off.
func (s *Server) serveFleet(w http.ResponseWriter, r *http.Request) {
	cams, err := s.fleet()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "fleet.html", struct {
		Title   string
		Cameras []Camera
	}{Title: "Cameras", Cameras: cams})
}
