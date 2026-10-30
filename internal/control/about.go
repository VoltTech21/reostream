package control

import (
	"net/http"

	"github.com/VoltTech21/reostream/internal/config"
)

// aboutPage is Settings > About: the build, the config file, and where each
// listener is, read from the config as it stands on disk.
type aboutPage struct {
	Title         string
	Version       string
	ConfigPath    string
	Listen        string
	RTSPListen    string
	ControlListen string
	Cameras       int
}

func (s *Server) serveAbout(w http.ResponseWriter, r *http.Request) {
	page := aboutPage{Title: "About", Version: buildVersion(), ConfigPath: s.opts.ConfigPath}
	if cfg, err := config.Load(s.opts.ConfigPath); err == nil {
		page.Listen = cfg.Listen
		if cfg.RTSP != nil {
			page.RTSPListen = cfg.RTSP.Listen
		}
		if cfg.Control != nil {
			page.ControlListen = cfg.Control.Listen
		}
		page.Cameras = len(cfg.Cameras)
	}
	s.render(w, r, "about.html", page)
}

func (s *Server) serveSettingsIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/password", http.StatusSeeOther)
}
