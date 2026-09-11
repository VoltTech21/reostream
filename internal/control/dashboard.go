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
