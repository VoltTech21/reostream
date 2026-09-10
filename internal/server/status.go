package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/VoltTech21/reostream/internal/stream"
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
// FPS, BitrateBps, LastFrameAgeSeconds and DroppedAudio come from
// hub.Hub.Stats(), which internal/stream updates as it publishes: the hub
// itself has no notion of a frame or a codec, only bytes to broadcast, so
// this is the one place that data is allowed to surface above it.
// LastFrameAgeSeconds is 0 only when there is no connection attempt in
// flight at all; a stream that is connected but has never delivered a
// frame reports its actual, growing time since connecting, not a
// permanent 0 (see hub.FrameStats.Age). A stream that never produced a
// frame reporting 0 forever, indistinguishable from one that just
// published, is exactly how two streams sat unnoticed for 25+ seconds in
// the 2026-09-08 incident this field's Streaming sibling exists to
// surface.
//
// DroppedAudio existing here at all is the point of exposing it: an ADPCM
// camera silently losing its audio, with the recorder's config still
// declaring an audio role, is exactly the failure that went unnoticed in
// production for days before anyone thought to check. A count that only
// lived inside a log line would have the same problem again.
//
// AudioFrames exists alongside DroppedAudio, not instead of it, because
// every stream now declares an audio track whether or not its camera ever
// sends one (see ts.NewMuxerWithAudio): DroppedAudio and AudioFrames both
// read 0 for a camera with no audio at all, exactly the same as for one
// whose audio is flowing perfectly, and an ffprobe of either looks
// identical too since the declared track is there regardless. AudioFrames
// > 0 is the only signal that audio is actually reaching a client;
// AudioFrames == 0 with DroppedAudio == 0 means no audio ever arrived,
// distinct from DroppedAudio > 0 meaning audio arrives but gets discarded.
// Streaming is deliberately its own field rather than a change to what
// Connected means. Connected already had an established meaning before this
// existed (the supervisor's goroutine for this stream is currently running,
// see supervisor.StreamStat.Running) and a dashboard built against that
// contract is entitled to keep reading it that way; changing Connected's
// truth condition out from under it, rather than adding a new field, is
// exactly the kind of silent field-meaning change the field name comment
// (below) warns against for LastFrameAgeSeconds. Streaming answers a
// narrower, newer question: is this connection actually delivering video
// right now, not just present. A stream can be Connected and not Streaming
// (a fresh connection still waiting on its first frame, or a held session
// the watchdog has not yet timed out), but never the reverse.
type StreamStatus struct {
	Connected           bool    `json:"connected"`
	Streaming           bool    `json:"streaming"`
	Clients             int     `json:"clients"`
	DroppedClients      int     `json:"dropped_clients"`
	Restarts            int     `json:"restarts"`
	LastError           string  `json:"last_error,omitempty"`
	FPS                 float64 `json:"fps"`
	BitrateBps          float64 `json:"bitrate_bps"`
	LastFrameAgeSeconds float64 `json:"last_frame_age_seconds"`
	AudioFrames         int     `json:"audio_frames"`
	DroppedAudio        int     `json:"dropped_audio"`
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
// the same way s.src is: "<camera>/<stream>".
func (s *Server) streamStats() map[string]StreamStatus {
	var bySup map[string]supervisor.StreamStat
	if s.sup != nil {
		bySup = make(map[string]supervisor.StreamStat)
		for _, st := range s.sup.Stats() {
			bySup[st.Camera+"/"+st.Stream] = st
		}
	}

	names := s.src.HubNames()
	out := make(map[string]StreamStatus, len(names))
	for _, name := range names {
		h, ok := s.src.Hub(name)
		if !ok {
			continue
		}
		fs := h.Stats()
		st := StreamStatus{
			Clients:             h.Clients(),
			DroppedClients:      h.Dropped(),
			FPS:                 fs.FPS,
			BitrateBps:          fs.BitrateBps,
			LastFrameAgeSeconds: fs.Age().Seconds(),
			AudioFrames:         fs.AudioFrames,
			DroppedAudio:        fs.DroppedAudio,
		}
		if sup, ok := bySup[name]; ok {
			st.Connected = sup.Running
			st.Restarts = sup.Restarts
			st.LastError = sup.LastError
		}
		// A stream reads as Streaming only while it is both connected and
		// recently delivering frames. stream.DefaultMediaTimeout is reused
		// here rather than a second, separately tuned constant, because it
		// is already the answer to exactly this question ("how stale can a
		// connection's last frame be before something is wrong"), derived
		// from the same fleet fps measurements; a status-only threshold
		// tuned differently from the one that actually tears the stream
		// down would just be a second number to keep in sync with the
		// first for no benefit. In steady state this rarely differs from
		// Connected: it is what a viewer sees in the brief window between a
		// stall starting and Run's own watchdog acting on it, and while a
		// fresh connection is still waiting on its first frame.
		st.Streaming = st.Connected && st.LastFrameAgeSeconds < stream.DefaultMediaTimeout.Seconds()
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

	writeGaugeHeader(&b, "reostream_stream_streaming", "1 if the stream is connected and has delivered a frame recently, 0 otherwise. Distinct from reostream_stream_connected: a held session can be connected without ever streaming.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_streaming{stream=%q} %d\n", name, boolToInt(stats[name].Streaming))
	}

	writeCounterHeader(&b, "reostream_stream_dropped_clients_total", "Subscribers disconnected for falling behind, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_dropped_clients_total{stream=%q} %d\n", name, stats[name].DroppedClients)
	}

	writeCounterHeader(&b, "reostream_stream_restarts_total", "Times the stream has reconnected after a failure, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_restarts_total{stream=%q} %d\n", name, stats[name].Restarts)
	}

	writeGaugeHeader(&b, "reostream_stream_fps", "Video frames per second, averaged over the last measurement window.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_fps{stream=%q} %g\n", name, stats[name].FPS)
	}

	writeGaugeHeader(&b, "reostream_stream_bitrate_bps", "MPEG-TS output bitrate, averaged over the last measurement window.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_bitrate_bps{stream=%q} %g\n", name, stats[name].BitrateBps)
	}

	writeGaugeHeader(&b, "reostream_stream_last_frame_age_seconds", "Time since the last frame was published, 0 if none ever was.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_last_frame_age_seconds{stream=%q} %g\n", name, stats[name].LastFrameAgeSeconds)
	}

	// audio_frames_total is what makes a silent camera diagnosable at all:
	// every stream declares an audio track regardless of whether its camera
	// sends one, so 0 dropped and 0 published together mean "no audio ever
	// arrives", distinct from audio arriving and being discarded (dropped >
	// 0) and from audio working normally (published > 0). Without this
	// series a silent camera and a healthy one report identically.
	writeCounterHeader(&b, "reostream_stream_audio_frames_total", "Audio frames actually muxed and published, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_audio_frames_total{stream=%q} %d\n", name, stats[name].AudioFrames)
	}

	writeCounterHeader(&b, "reostream_stream_dropped_audio_total", "Audio frames dropped for lacking a supported codec, since start.")
	for _, name := range names {
		fmt.Fprintf(&b, "reostream_stream_dropped_audio_total{stream=%q} %d\n", name, stats[name].DroppedAudio)
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
