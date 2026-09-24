package control

import (
	"fmt"
	"sort"
	"strings"
)

// cameraGroup is one camera's line on the dashboard: every stream it
// reports status for, plus the single video tile the operator can start
// for it. Grouping by camera, rather than rendering the stats table and
// the tile grid as two separate sections, is what puts a camera's numbers
// next to the picture they describe.
type cameraGroup struct {
	Camera string
	Rows   []row
	Tile   tile
}

// cameraGroups joins s.rows() (one entry per camera/stream, e.g.
// "driveway/main") with s.tiles() (one entry per camera) on the camera
// name. A camera with no tile yet (Hubs is nil, or nothing has reported a
// codec) still gets a group with a zero-value Tile, which renders as "not
// playable" rather than dropping the camera's stats row.
func (s *Server) cameraGroups() []cameraGroup {
	rows := s.rows()
	tiles := s.tiles()

	tileByCamera := make(map[string]tile, len(tiles))
	for _, t := range tiles {
		tileByCamera[t.Camera] = t
	}

	order := make([]string, 0)
	seen := make(map[string]bool)
	rowsByCamera := make(map[string][]row)
	for _, r := range rows {
		camera, _, _ := strings.Cut(r.Name, "/")
		rowsByCamera[camera] = append(rowsByCamera[camera], r)
		if !seen[camera] {
			seen[camera] = true
			order = append(order, camera)
		}
	}
	for _, t := range tiles {
		if !seen[t.Camera] {
			seen[t.Camera] = true
			order = append(order, t.Camera)
		}
	}
	sort.Strings(order)

	out := make([]cameraGroup, 0, len(order))
	for _, camera := range order {
		out = append(out, cameraGroup{
			Camera: camera,
			Rows:   rowsByCamera[camera],
			Tile:   tileByCamera[camera],
		})
	}
	return out
}

// streamLine is one stream inside a camera's card: the same row the status
// table has always carried, plus the bare stream name ("main", not
// "lounge/main") and the URL a recorder would point at it.
type streamLine struct {
	row
	Stream string
	URL    string
	// Codec is what the stream's last keyframe was, "h264" or "h265", and
	// empty for a stream that has not delivered one. It is the fact that
	// decides whether a browser can show this stream at all.
	Codec string
}

// statusCard is one CAMERA on the status page, not one stream. A camera
// pulling main, sub and extern used to render as three separate cards,
// which is the same "two different things called a camera" confusion the
// merge of the operator page and the camera control page removed: a person
// has one camera in their head. Everything that camera is doing -- every
// stream's state, its address, its URLs, its picture, its overlay toggles
// -- belongs in one card.
type statusCard struct {
	Camera  string
	Address string
	Streams []streamLine
	Tile    tile
	// State is the camera's one pill: the worst of its streams, so a
	// camera with one sick stream does not read as healthy.
	State string
}

// statusCards folds the per-stream rows into one card per camera, joining
// in each camera's configured address and each stream's recorder URL.
//
// Nothing here contacts a camera. Every value comes from memory (the
// stream stats and the hubs) or from the config file, which is what makes
// this page render instantly however many cameras are configured; see
// dashboard.html for how the overlay toggles keep that true.
func (s *Server) statusCards(urls RecorderURLs) []statusCard {
	// Name and Address only. Camera also carries Username and Password,
	// and no camera password may reach a template, a response body, a log
	// line or a URL.
	address := make(map[string]string)
	if cams, err := s.fleet(); err == nil {
		for _, c := range cams {
			address[c.Name] = c.Address
		}
	}
	// A fleet that will not load costs this page its addresses, not the
	// page: the stream state is read from memory and is still worth
	// showing, and the config page is where a broken config gets reported.

	// Every configured camera gets a card, including one nothing has
	// reported status for yet: a camera that is in the config and missing
	// from this page reads as "gone", which is the opposite of what a
	// camera that has not connected yet needs to look like.
	groups := s.cameraGroups()
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		seen[g.Camera] = true
	}
	for name := range address {
		if !seen[name] {
			groups = append(groups, cameraGroup{Camera: name})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Camera < groups[j].Camera })

	codecs := s.codecsByCamera()
	out := make([]statusCard, 0, len(groups))
	for _, g := range groups {
		card := statusCard{Camera: g.Camera, Address: address[g.Camera], Tile: g.Tile}
		for _, r := range g.Rows {
			_, stream, _ := strings.Cut(r.Name, "/")
			card.Streams = append(card.Streams, streamLine{
				row:    r,
				Stream: stream,
				URL:    urls.StreamHTTP[g.Camera][stream],
				Codec:  codecs[g.Camera][stream],
			})
		}
		// main, then sub, then extern. Alphabetical put extern above main,
		// which reads as though the balanced stream were the important one.
		sort.SliceStable(card.Streams, func(i, j int) bool {
			return streamRank(card.Streams[i].Stream) < streamRank(card.Streams[j].Stream)
		})
		card.State = cameraState(g.Rows)
		out = append(out, card)
	}
	return out
}

// streamRank orders a camera's streams the way an operator reads them:
// the main picture first, then the small one, then the balanced one.
// Anything unrecognised sorts after those, keeping its own order.
func streamRank(name string) int {
	switch name {
	case "main":
		return 0
	case "sub":
		return 1
	case "extern":
		return 2
	}
	return 3
}

// stateRank orders stream states worst first for cameraState.
var stateRank = map[string]int{"down": 4, "reconnecting": 3, "novideo": 2, "streaming": 1}

// cameraState is the worst state among a camera's streams, or "idle" for
// a camera with none reported.
func cameraState(rows []row) string {
	worst := "idle"
	for _, r := range rows {
		if stateRank[r.State] > stateRank[worst] {
			worst = r.State
		}
	}
	return worst
}

// railEntry is one camera in the sidebar: a name and a state colour.
type railEntry struct {
	Name  string
	State string
}

// railEntries pairs each configured camera with its state, read from
// memory. It never contacts a camera, since it runs on every page render.
func railEntries(cams []Camera, status StatusSource) []railEntry {
	byCamera := make(map[string][]row)
	if status != nil {
		for name, st := range status.StreamStats() {
			camera, _, _ := strings.Cut(name, "/")
			byCamera[camera] = append(byCamera[camera], row{Name: name, State: streamState(st)})
		}
	}
	out := make([]railEntry, 0, len(cams))
	for _, c := range cams {
		out = append(out, railEntry{Name: c.Name, State: cameraState(byCamera[c.Name])})
	}
	return out
}

// Alert is a fleet-level line above the dashboard grid, one per camera
// that is down: a plain sentence and the exact detail behind it.
type Alert struct {
	Camera   string
	Sentence string
	Detail   string
}

// alertAfter is how long a reconnecting camera may go without a frame
// before it is called out above the grid. A stream only reads "down" when
// it has never restarted, so a camera that dropped and keeps retrying is
// "reconnecting" for as long as it is gone; without this, the camera most
// worth an alert would never get one.
const alertAfter = 60 // seconds

// alerts is one line per camera that is down, or reconnecting and silent
// for alertAfter or longer.
func alerts(cards []statusCard) []Alert {
	var out []Alert
	for _, c := range cards {
		if c.State != "down" && c.State != "reconnecting" {
			continue
		}
		var age float64
		var restarts int
		var lastErr string
		for _, st := range c.Streams {
			if st.State == "streaming" || st.State == "novideo" {
				continue
			}
			age = max(age, st.LastFrameAgeSeconds)
			restarts += st.Restarts
			if lastErr == "" {
				lastErr = st.LastError
			}
		}
		if c.State == "reconnecting" && age < alertAfter {
			continue
		}
		sentence := c.Camera + " is not connected."
		if mins := int(age / 60); mins >= 1 {
			sentence = fmt.Sprintf("%s has recorded nothing for %d minutes.", c.Camera, mins)
		}
		if int(age/60) == 1 {
			sentence = c.Camera + " has recorded nothing for 1 minute."
		}
		detail := fmt.Sprintf("%d restarts", restarts)
		if restarts == 1 {
			detail = "1 restart"
		}
		if lastErr != "" {
			detail = lastErr + " · " + detail
		}
		out = append(out, Alert{Camera: c.Camera, Sentence: sentence, Detail: detail})
	}
	return out
}

// healthyCount is how many cameras are streaming, for the page subtitle.
func healthyCount(cards []statusCard) int {
	n := 0
	for _, c := range cards {
		if c.State == "streaming" {
			n++
		}
	}
	return n
}
