// Package server exposes hub streams as MPEG-TS over HTTP.
package server

import (
	"net/http"
	"strings"

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

	// sup is optional; see SetSupervisor in status.go.
	sup *supervisor.Supervisor
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
	mux.HandleFunc("/", s.serveStreamOrKeyframe)
	return mux
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
	for suffix, stream := range map[string]string{"_sub": "sub", "_extern": "extern"} {
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
		http.NotFound(w, r)
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
