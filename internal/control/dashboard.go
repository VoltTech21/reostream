package control

import (
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

	out := make([]statusCard, 0, len(groups))
	for _, g := range groups {
		card := statusCard{Camera: g.Camera, Address: address[g.Camera], Tile: g.Tile}
		for _, r := range g.Rows {
			_, stream, _ := strings.Cut(r.Name, "/")
			card.Streams = append(card.Streams, streamLine{
				row:    r,
				Stream: stream,
				URL:    urls.StreamHTTP[g.Camera][stream],
			})
		}
		out = append(out, card)
	}
	return out
}
