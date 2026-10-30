package control

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
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
	// StreamRTSP is the RTSP output per camera and stream, for the streams
	// the config serves over RTSP. Empty when RTSP is off.
	StreamRTSP map[string]map[string]string
	// FrigateInputs is each camera's Frigate ffmpeg inputs, in Cameras
	// order, for the snippet and for updating a pasted config.
	FrigateInputs map[string][]frigateInput
	Cameras       []string
	// RTSPHost and RTSPPort are the RTSP listener as a recorder dials it;
	// RTSPPort is empty when RTSP is off.
	RTSPHost, RTSPPort string
	// LoginUser and LoginPassword are the [rtsp] login every URL above
	// already carries, for a recorder that asks for them in fields of
	// their own instead (Blue Iris). Empty when no login is set.
	LoginUser, LoginPassword string
}

// httpStreamURL is where the streaming listener serves one stream: main is
// the camera's bare name, and every other stream carries its own name as a
// suffix. It is the single place that shape is written down. login is the
// "user:password@" a guarded listener needs, or empty.
func httpStreamURL(login, base, camera, stream string) string {
	if stream == "main" {
		return fmt.Sprintf("http://%s%s/%s.ts", login, base, camera)
	}
	return fmt.Sprintf("http://%s%s/%s_%s.ts", login, base, camera, stream)
}

// streamLogin is the [rtsp] login as the userinfo part of a URL,
// "user:password@", escaped the way a URL needs it, and the login itself.
// Every URL a recorder is handed carries it, since one pasted without it
// is a 401 that looks like a broken stream.
//
// The config may be LoadRaw's, with "$NAME" still in it. This process
// resolved the same reference at startup, so the environment has it; one
// that does not resolve is shown as written rather than as nothing.
func streamLogin(cfg *config.Config) (userinfo, user, pass string) {
	user, pass, ok := cfg.RTSP.Credentials()
	if !ok {
		return "", "", ""
	}
	user, pass = fromEnv(user), fromEnv(pass)
	return url.UserPassword(user, pass).String() + "@", user, pass
}

// fromEnv resolves a "$NAME" config value from the environment.
func fromEnv(v string) string {
	if name, ok := strings.CutPrefix(v, "$"); ok {
		if val, set := os.LookupEnv(name); set {
			return val
		}
	}
	return v
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
	out := RecorderURLs{StreamHTTP: make(map[string]map[string]string, len(cfg.Cameras)), StreamRTSP: make(map[string]map[string]string), FrigateInputs: map[string][]frigateInput{}, RTSPHost: host}
	login, user, pass := streamLogin(cfg)
	out.LoginUser, out.LoginPassword = user, pass
	if cfg.RTSP != nil {
		out.RTSPPort = portOf(cfg.RTSP.Listen)
	}
	var frigate strings.Builder
	frigate.WriteString("cameras:\n")

	for _, cam := range cfg.Cameras {
		main := httpStreamURL(login, httpBase, cam.Name, "main")
		out.HTTP = append(out.HTTP, main)

		byStream := make(map[string]string, len(cam.Streams))
		for _, s := range cam.Streams {
			byStream[s] = httpStreamURL(login, httpBase, cam.Name, s)
		}
		out.StreamHTTP[cam.Name] = byStream

		// Detect uses the sub stream when the camera has one; falling back
		// to main rather than emitting a URL for a stream this camera never
		// carries a frame on.
		detect := main
		for _, s := range cam.Streams {
			if s == "sub" {
				detect = httpStreamURL(login, httpBase, cam.Name, "sub")
			}
		}
		out.Cameras = append(out.Cameras, cam.Name)
		inputs := []frigateInput{{detect, []string{"detect"}}, {main, []string{"record", "audio"}}}
		if detect == main {
			// One stream doing all three: Frigate refuses the same path twice.
			inputs = []frigateInput{{main, []string{"detect", "record", "audio"}}}
		}
		out.FrigateInputs[cam.Name] = inputs
		fmt.Fprintf(&frigate, "  %s:\n    ffmpeg:\n      inputs:\n", cam.Name)
		for _, in := range inputs {
			fmt.Fprintf(&frigate, "        - path: %s\n          roles: [%s]\n", in.Path, strings.Join(in.Roles, ", "))
		}

		if cfg.RTSP != nil {
			rtspBase := net.JoinHostPort(host, portOf(cfg.RTSP.Listen))
			for _, s := range cam.RTSP {
				suffix := ""
				if s != "main" {
					suffix = "_" + s
				}
				u := fmt.Sprintf("rtsp://%s%s/%s%s", login, rtspBase, cam.Name, suffix)
				out.RTSP = append(out.RTSP, u)
				if out.StreamRTSP[cam.Name] == nil {
					out.StreamRTSP[cam.Name] = map[string]string{}
				}
				out.StreamRTSP[cam.Name][s] = u
			}
		}
	}
	out.Frigate = frigate.String()
	return out
}

func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request) {
	// Ranges is what the sweep button will offer. Reading the interface
	// list is local and opens no connection, so it is safe to do on every
	// page load; the sweep itself is not, which is why the button posts
	// rather than this page dialling anything. More than one means this
	// machine is on more than one private network and only the operator
	// knows which one the cameras are on, so the page asks rather than
	// picking.
	auto := autoSweepRanges(r, s.opts.ConfigPath)
	autoJSON, err := json.Marshal(auto)
	if err != nil {
		autoJSON = []byte("[]")
	}
	prefill := ""
	if len(auto) > 0 {
		prefill = auto[0]
	} else if ranges := localSweepRanges(); len(ranges) > 0 {
		prefill = ranges[0]
	}
	s.render(w, r, "setup.html", struct {
		Title          string
		AutoRangesJSON template.JS
		Prefill        string
	}{Title: "Setup", AutoRangesJSON: template.JS(autoJSON), Prefill: prefill})
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
		s.render(w, r, "urls.html", page)
		return
	}
	// The host the operator reached this page on, not the config's listen
	// address: that is normally a wildcard, and a URL built from 0.0.0.0
	// works from nowhere.
	page.URLs = recorderURLs(cfg, pageHost(r))
	s.render(w, r, "urls.html", page)
}

// nvrPage is Settings > Connect to NVR.
type nvrPage struct {
	Title string
	URLs  RecorderURLs
	Error string
	// Tab is the recorder shown first.
	Tab string
	// Pasted, Merged, Updated, Added and MergeErr are the Frigate config
	// updater's input and result.
	Pasted, Merged string
	Updated, Added []string
	MergeErr       string
}

// RTSPPath is a camera stream's path on the RTSP listener, as Blue Iris
// and similar recorders ask for it.
func (p nvrPage) RTSPPath(cam, stream string) string {
	if stream == "main" {
		return "/" + cam
	}
	return "/" + cam + "_" + stream
}

// serveNVR shows the recorder snippets; a POST runs the Frigate config
// updater on a pasted config.
func (s *Server) serveNVR(w http.ResponseWriter, r *http.Request) {
	page := nvrPage{Title: "Connect to NVR", Tab: "frigate"}
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		s.render(w, r, "nvr.html", page)
		return
	}
	page.URLs = recorderURLs(cfg, pageHost(r))
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		page.Pasted = r.FormValue("frigate")
		out, updated, added, err := mergeFrigate([]byte(page.Pasted), page.URLs.FrigateInputs, page.URLs.Cameras)
		if err != nil {
			page.MergeErr = err.Error()
		} else {
			page.Merged, page.Updated, page.Added = string(out), updated, added
		}
	}
	s.render(w, r, "nvr.html", page)
}

// hostOnly strips the port from a Host header, which carries one whenever
// the page is not on 80 or 443, and this page never is.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
