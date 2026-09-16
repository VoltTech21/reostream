package control

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/VoltTech21/reostream/internal/config"
)

// RecorderURLs is the paste-ready output of setup. The Frigate block uses
// the sub stream for detect and main for record and audio, which is what
// the README documents and what the fleet actually runs.
type RecorderURLs struct {
	Frigate string
	RTSP    []string
	HTTP    []string

	// StreamHTTP is the same HTTP output, but every stream a camera
	// carries rather than only its main, keyed by camera name and then
	// stream name. The status page shows a stream's URL next to that
	// stream's own state, and it reads it from here rather than building
	// one itself: two builders for the same URL drift, and the one an
	// operator pastes into a recorder must be the one the page shows.
	StreamHTTP map[string]map[string]string
}

// httpStreamURL is where the streaming listener serves one stream: main is
// the camera's bare name, and every other stream carries its own name as a
// suffix. It is the single place that shape is written down.
func httpStreamURL(base, camera, stream string) string {
	if stream == "main" {
		return fmt.Sprintf("http://%s/%s.ts", base, camera)
	}
	return fmt.Sprintf("http://%s/%s_%s.ts", base, camera, stream)
}

// portOf takes the port from a listen address, which is normally written
// with a wildcard host that is not reachable from anywhere.
func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return listen
}

func recorderURLs(cfg *config.Config, host string) RecorderURLs {
	// net.JoinHostPort, not a bare "%s:%s": host can be a literal IPv6
	// address, and a raw "host:port" interpolation of one produces a URL
	// with an unbracketed colon-separated address, which nothing parses as
	// the host and port they are meant to be.
	httpBase := net.JoinHostPort(host, portOf(cfg.Listen))
	out := RecorderURLs{StreamHTTP: make(map[string]map[string]string, len(cfg.Cameras))}
	var frigate strings.Builder
	frigate.WriteString("cameras:\n")

	for _, cam := range cfg.Cameras {
		main := httpStreamURL(httpBase, cam.Name, "main")
		out.HTTP = append(out.HTTP, main)

		byStream := make(map[string]string, len(cam.Streams))
		for _, s := range cam.Streams {
			byStream[s] = httpStreamURL(httpBase, cam.Name, s)
		}
		out.StreamHTTP[cam.Name] = byStream

		// Detect uses the sub stream when the camera has one; falling back
		// to main rather than emitting a URL for a stream this camera never
		// carries a frame on.
		detect := main
		for _, s := range cam.Streams {
			if s == "sub" {
				detect = httpStreamURL(httpBase, cam.Name, "sub")
			}
		}
		fmt.Fprintf(&frigate, "  %s:\n    ffmpeg:\n      inputs:\n", cam.Name)
		fmt.Fprintf(&frigate, "        - path: %s\n          roles: [detect]\n", detect)
		fmt.Fprintf(&frigate, "        - path: %s\n          roles: [record, audio]\n", main)

		if cfg.RTSP != nil {
			rtspBase := net.JoinHostPort(host, portOf(cfg.RTSP.Listen))
			for _, s := range cam.RTSP {
				suffix := ""
				if s != "main" {
					suffix = "_" + s
				}
				out.RTSP = append(out.RTSP,
					fmt.Sprintf("rtsp://%s/%s%s", rtspBase, cam.Name, suffix))
			}
		}
	}
	out.Frigate = frigate.String()
	return out
}

func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request) {
	s.render(w, "setup.html", struct{ Title string }{Title: "Setup"})
}

func (s *Server) serveURLs(w http.ResponseWriter, r *http.Request) {
	page := struct {
		Title string
		URLs  RecorderURLs
		Error string
	}{Title: "URLs"}

	// LoadRaw, not Load: this page only reads the config to build URLs, and
	// Load would require every camera's "$NAME" password reference to have
	// a real environment variable set just to compute a hostname and a
	// port, which this page never touches.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		s.render(w, "urls.html", page)
		return
	}
	// The host the operator reached this page on, not the config's listen
	// address: that is normally a wildcard, and a URL built from 0.0.0.0
	// works from nowhere.
	page.URLs = recorderURLs(cfg, hostOnly(r.Host))
	s.render(w, "urls.html", page)
}

// hostOnly strips the port from a Host header, which carries one whenever
// the page is not on 80 or 443, and this page never is.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
