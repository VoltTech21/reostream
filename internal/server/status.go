package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/VoltTech21/reostream/internal/supervisor"
)

// StreamStatus is one stream's entry in GET /api/status.
//
// Restarts and LastError are only populated once a Supervisor has been
// attached with SetSupervisor; a Server built for tests or for the single
// camera command form reports zero values for both rather than omitting
// the fields, which would make "never restarted" indistinguishable from
// "no supervisor is wired up".
//
// FPS, bitrate, last-frame age and dropped-audio count are not reported
// here yet: producing them needs stream.Run to surface its per-connection
// ts.Muxer (for DroppedAudio) and a running rate, which no current caller
// exposes. Adding those is follow-up work, not a reason to hold back the
// fields this data already supports.
type StreamStatus struct {
	Connected      bool   `json:"connected"`
	Clients        int    `json:"clients"`
	DroppedClients int    `json:"dropped_clients"`
	Restarts       int    `json:"restarts"`
	LastError      string `json:"last_error,omitempty"`
}

// statusBody is the shape of GET /api/status.
type statusBody struct {
	Streams map[string]StreamStatus `json:"streams"`
}

// SetSupervisor attaches sup so status and metrics can report whether each
// stream is actually connected, not just how many hub subscribers it has.
// Optional: a Server with no supervisor attached still lists every stream,
// with Connected and Restarts reading false and 0 rather than the endpoint
// erroring or omitting the entry, which is what TestStatusReportsEveryStream
// guards against a stream that never got wired up.
func (s *Server) SetSupervisor(sup *supervisor.Supervisor) {
	s.sup = sup
}

// streamStats builds the current status for every configured stream, keyed
// the same way s.streams is: "<camera>/<stream>".
func (s *Server) streamStats() map[string]StreamStatus {
	var bySup map[string]supervisor.StreamStat
	if s.sup != nil {
		bySup = make(map[string]supervisor.StreamStat)
		for _, st := range s.sup.Stats() {
			bySup[st.Camera+"/"+st.Stream] = st
		}
	}

	out := make(map[string]StreamStatus, len(s.streams))
	for name, h := range s.streams {
		st := StreamStatus{
			Clients:        h.Clients(),
			DroppedClients: h.Dropped(),
		}
		if sup, ok := bySup[name]; ok {
			st.Connected = sup.Running
			st.Restarts = sup.Restarts
			st.LastError = sup.LastError
		}
		out[name] = st
	}
	return out
}

func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statusBody{Streams: s.streamStats()})
}

// serveMetrics writes Prometheus text exposition format. It is built by
// hand rather than pulled in from client_golang: this process exports four
// gauges and a counter, which is not worth a dependency and its own
// registry plumbing.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.streamStats()
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	// Sorted so the output is stable between scrapes, which is friendlier
	// to a human reading it with curl than a Go map's random order.
	sort.Strings(names)

	var b strings.Builder
	writeGaugeHeader(&b, "reostream_stream_clients", "Current subscriber count for a stream.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_clients{stream=%q} %d\n", name, stats[name].Clients)
	}

	writeGaugeHeader(&b, "reostream_stream_connected", "1 if the stream is currently connected to its camera, 0 otherwise.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_connected{stream=%q} %d\n", name, boolToInt(stats[name].Connected))
	}

	writeCounterHeader(&b, "reostream_stream_dropped_clients_total", "Subscribers disconnected for falling behind, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_dropped_clients_total{stream=%q} %d\n", name, stats[name].DroppedClients)
	}

	writeCounterHeader(&b, "reostream_stream_restarts_total", "Times the stream has reconnected after a failure, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_restarts_total{stream=%q} %d\n", name, stats[name].Restarts)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
}

func writeGaugeHeader(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
}

func writeCounterHeader(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
