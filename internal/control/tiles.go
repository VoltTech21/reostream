package control

import (
	"sort"
	"strings"
)

// browserPlayable is the codec set a browser will decode from an MPEG-TS
// stream through Media Source Extensions. HEVC is absent deliberately: the
// main streams on this fleet are HEVC and no browser here plays them, which
// is why a tile picks a stream rather than always using main.
func browserPlayable(codec string) bool {
	return strings.EqualFold(codec, "H264")
}

// playableStream chooses the stream a tile should play for one camera, and
// returns the path on the streaming listener. Preference is sub then
// extern: sub is the smallest, and extern is 896x512 H.264 at roughly
// 1 Mbps, which covers models whose substream is also HEVC.
//
// It returns false rather than guessing when nothing is playable, including
// when a stream has not yet reported a codec, because a player pointed at a
// stream it cannot decode looks like a broken camera.
func playableStream(camera string, streams map[string]string) (string, bool) {
	for _, name := range []string{"sub", "extern"} {
		if browserPlayable(streams[name]) {
			switch name {
			case "sub":
				return "/" + camera + "_sub.ts", true
			case "extern":
				return "/" + camera + "_extern.ts", true
			}
		}
	}
	return "", false
}

// codecsByCamera reads the codec each stream last reported, grouped by
// camera. A stream with no keyframe yet reports "".
func (s *Server) codecsByCamera() map[string]map[string]string {
	out := make(map[string]map[string]string)
	if s.opts.Hubs == nil {
		return out
	}
	for _, key := range s.opts.Hubs.HubNames() {
		camera, stream, found := strings.Cut(key, "/")
		if !found {
			continue
		}
		h, ok := s.opts.Hubs.Hub(key)
		if !ok {
			continue
		}
		codec, _ := h.Keyframe()
		if out[camera] == nil {
			out[camera] = make(map[string]string)
		}
		out[camera][stream] = codec
	}
	return out
}

// tile is one camera's live picture on the dashboard.
type tile struct {
	Camera   string
	URL      string
	Playable bool
}

func (s *Server) tiles() []tile {
	byCam := s.codecsByCamera()
	names := make([]string, 0, len(byCam))
	for cam := range byCam {
		names = append(names, cam)
	}
	sort.Strings(names)
	out := make([]tile, 0, len(names))
	for _, cam := range names {
		path, ok := playableStream(cam, byCam[cam])
		out = append(out, tile{
			Camera:   cam,
			URL:      s.opts.StreamBase + path,
			Playable: ok,
		})
	}
	return out
}
