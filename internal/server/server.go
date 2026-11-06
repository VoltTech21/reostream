// Package server exposes hub streams as MPEG-TS over HTTP.
package server

import (
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// HubSource supplies the hubs a Server routes to. It is an interface rather
// than a map because reload adds and removes streams while the server is
// running, so a snapshot taken at construction goes stale the first time a
// camera is added.
type HubSource interface {
	Hub(name string) (*hub.Hub, bool)
	HubNames() []string
}

// staticHubs is a HubSource over a fixed map, for callers with no
// supervisor: tests, and the single camera command form.
type staticHubs map[string]*hub.Hub

func (s staticHubs) Hub(name string) (*hub.Hub, bool) {
	h, ok := s[name]
	return h, ok
}

func (s staticHubs) HubNames() []string {
	out := make([]string, 0, len(s))
	for name := range s {
		out = append(out, name)
	}
	return out
}

// StaticHubs wraps a fixed hub map as a HubSource.
func StaticHubs(m map[string]*hub.Hub) HubSource { return staticHubs(m) }

// Server routes GET /<cam>.ts, /<cam>_sub.ts and /<cam>_extern.ts to the
// matching hub, plus /api/status and /metrics for the whole fleet.
type Server struct {
	src HubSource

	// hinted marks the paths notFound has logged its hint for, so a
	// recorder retrying every few seconds logs it once.
	hintMu sync.Mutex
	hinted map[string]bool

	// sup is optional; see SetSupervisor in status.go.
	sup *supervisor.Supervisor

	// wake lets a request subscribe to a battery camera's on-demand hub,
	// which wakes the camera. Only the signed-in control listener sets it.
	wake bool
}

// AllowWake lets this server's viewers wake battery cameras. Only for a
// listener behind the control password: an open port would let any
// recorder hold a battery camera awake.
func (s *Server) AllowWake() *Server {
	s.wake = true
	return s
}

// New builds a Server over src. The header each client receives on connect
// comes from the hub itself (hub.Header), not from a value passed in here:
// the header is the muxer's PAT/PMT pair, which does not exist until the
// muxer has seen a first frame and learned the codec, which is after the
// server is constructed.
func New(src HubSource) *Server {
	return &Server{src: src}
}

// Handler returns the HTTP handler serving all configured streams.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.serveStatus)
	mux.HandleFunc("/metrics", s.serveMetrics)
	// Both stream and keyframe routes are dispatched from one catch-all
	// rather than two ServeMux patterns: the stdlib mux's {wildcard}
	// segments have to occupy a whole path segment, so a pattern like
	// "/{name}.keyframe" is not expressible and this is simpler than a
	// second mux layered on top.
	mux.Handle("/", s.StreamHandler())
	return mux
}

// StreamHandler returns only the per-camera video and keyframe routes,
// without /api/status or /metrics. It exists so a second listener can offer
// the same hub subscriptions without also exposing the status endpoints a
// second time; see internal/control's mount of this under /stream/, which
// is what makes the operator page's live tiles same-origin.
func (s *Server) StreamHandler() http.Handler {
	return http.HandlerFunc(s.serveStreamOrKeyframe)
}

// lookup resolves a request path stem, meaning the path with its leading
// slash and its ".ts" or ".keyframe" suffix removed, onto a hub.
//
// Hubs are keyed "<camera>/<stream>" (see supervisor.hubName), but the URL
// form is the flat one documented in the README: a bare camera name is its
// main stream, and "_sub" or "_extern" selects the other two. That form is
// what any recorder config written against this project already contains,
// and a URL is a published interface where an internal map key is not, so
// the routing bends to the URL rather than the other way round. The hub key
// itself is still accepted, since it is what an operator reading /api/status
// sees.
//
// Resolution order is exact key, then bare name as main, then the suffix
// rules. That only matters for a camera literally named something like
// "gate_sub", where it means the real camera wins over a suffix reading of
// its name.
func (s *Server) lookup(stem string) (*hub.Hub, bool) {
	if h, ok := s.src.Hub(stem); ok {
		return h, true
	}
	if h, ok := s.src.Hub(stem + "/main"); ok {
		return h, true
	}
	for suffix, stream := range map[string]string{"_sub": "sub", "_extern": "extern", "_zoom": "zoom", "_zoom_sub": "zoom_sub"} {
		if cam, cut := strings.CutSuffix(stem, suffix); cut {
			if h, ok := s.src.Hub(cam + "/" + stream); ok {
				return h, true
			}
		}
	}
	return nil, false
}

// serveStreamOrKeyframe dispatches on the path suffix, since the stdlib
// mux's wildcard segments can't express "/<name>.keyframe" alongside
// "/<name>.ts" as separate registered patterns (see Handler).
func (s *Server) serveStreamOrKeyframe(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, ".still.ts") {
		s.serveStill(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".keyframe") {
		s.serveKeyframe(w, r)
		return
	}
	s.serveStream(w, r)
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasSuffix(r.URL.Path, ".ts") {
		http.NotFound(w, r)
		return
	}
	h, ok := s.lookup(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	if !ok {
		s.notFound(w, r)
		return
	}
	if h.OnDemand() && !s.wake {
		http.Error(w, "battery camera: it streams only while watched in reostream", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Subscribe before writing anything, so a frame published between the
	// header write and the loop starting is not missed.
	ch, cancel := h.Subscribe()
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// WriteHeader alone only buffers the status line; without an explicit
	// flush here the client's Do() blocks waiting for headers that never
	// go out until the first Write, which for a stream with no cached
	// header and no traffic yet never comes.
	flusher.Flush()

	if hdr := h.Header(); hdr != nil {
		if _, err := w.Write(hdr); err != nil {
			return
		}
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, open := <-ch:
			if !open {
				return
			}
			// Without the flush, a low bitrate substream looks like a hang
			// until Go's write buffer happens to fill on its own, which can
			// take seconds.
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// streamPaths maps a hub's stream to the ending of its .ts URL; main is the
// bare camera name.
var streamPaths = []struct{ suffix, stream string }{
	{"_zoom_sub", "zoom_sub"}, {"_zoom", "zoom"}, {"_sub", "sub"}, {"_extern", "extern"}, {"", "main"},
}

// notFound answers a .ts path no hub serves. When the camera it names has
// other streams, the 404 says which and how to turn the asked-for one on,
// as the RTSP 404 has since 11-02: a plain 404 left a recorder pointed at
// /cam-19_zoom_sub.ts with nothing to go on. The log says it once per path.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	stem := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
	names := s.src.HubNames()
	for _, sp := range streamPaths {
		cam, cut := strings.CutSuffix(stem, sp.suffix)
		if !cut || cam == "" {
			continue
		}
		// Every stream turned on, in the order and words the RTSP 404 uses
		// (config.ServedList), with the ones the camera has sent nothing
		// for yet marked so.
		var served []config.ServedPath
		for _, other := range config.NotServedOrder {
			if !slices.Contains(names, cam+"/"+other.Stream) {
				continue
			}
			video := false
			if h, ok := s.src.Hub(cam + "/" + other.Stream); ok {
				video = !h.Stats().LastFrameAt.IsZero()
			}
			served = append(served, config.ServedPath{Path: "/" + cam + other.Suffix + ".ts", Video: video})
		}
		if len(served) == 0 {
			continue
		}
		list := config.ServedList(served)
		how := config.HowToServe(sp.stream)
		s.hintMu.Lock()
		first := !s.hinted[stem]
		if s.hinted == nil {
			s.hinted = map[string]bool{}
		}
		s.hinted[stem] = true
		s.hintMu.Unlock()
		if first {
			log.Printf("http: /%s.ts is not served; this camera serves %s. %s", stem, list, how)
		}
		http.Error(w, "404 page not found: this camera serves "+list+". "+how+".", http.StatusNotFound)
		return
	}
	http.NotFound(w, r)
}
