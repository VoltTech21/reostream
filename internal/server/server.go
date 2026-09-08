// Package server exposes hub streams as MPEG-TS over HTTP.
package server

import (
	"net/http"
	"strings"

	"github.com/VoltTech21/reostream/internal/hub"
)

// Server routes GET /<name>.ts to the matching hub.
type Server struct {
	streams map[string]*hub.Hub
}

// New builds a Server over the given named streams. The header each client
// receives on connect comes from the hub itself (hub.Header), not from a
// value passed in here: the header is the muxer's PAT/PMT pair, which does
// not exist until the muxer has seen a first frame and learned the codec,
// which is after the server is constructed.
func New(streams map[string]*hub.Hub) *Server {
	return &Server{streams: streams}
}

// Handler returns the HTTP handler serving all configured streams.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveStream)
	return mux
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
	h, ok := s.streams[name]
	if !ok || !strings.HasSuffix(r.URL.Path, ".ts") {
		http.NotFound(w, r)
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
