package control

import (
	"fmt"
	"sort"

	"github.com/VoltTech21/reostream/internal/server"
)

// StatusSource is the daemon's live stream status. It is an interface so
// this package does not construct or own anything in the streaming path.
type StatusSource interface {
	StreamStats() map[string]server.StreamStatus
}

// streamState reduces a StreamStatus to the state an operator asks about.
//
// The Connected and Streaming distinction is the reason StreamStatus has
// both fields, and a reader will not infer it from two booleans in a table.
// "novideo" is a connection that is up and delivering nothing, which is what
// a camera holding a dead session looks like from here.
func streamState(st server.StreamStatus) string {
	switch {
	case st.Connected && st.Streaming:
		return "streaming"
	case st.Connected:
		return "novideo"
	case st.Restarts > 0:
		return "reconnecting"
	default:
		return "down"
	}
}

// row is one line of the dashboard.
type row struct {
	Name  string
	State string
	server.StreamStatus
}

// MbpsBitrate renders BitrateBps for a person, not a scraper: /api/status
// and /metrics keep the raw bits-per-second value exactly as they always
// have, since a recorder and Prometheus both parse it, but a human reading
// the dashboard wants "6.2 Mbps", not "6231488".
func (r row) MbpsBitrate() string {
	return fmt.Sprintf("%.1f", r.BitrateBps/1e6)
}

func (s *Server) rows() []row {
	if s.opts.Status == nil {
		return nil
	}
	stats := s.opts.Status.StreamStats()
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]row, 0, len(names))
	for _, name := range names {
		st := stats[name]
		out = append(out, row{Name: name, State: streamState(st), StreamStatus: st})
	}
	return out
}
