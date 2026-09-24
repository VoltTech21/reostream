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
// externStream returns the balanced stream's path for camera, or "" when
// it serves none a browser can play.
func externStream(camera string, streams map[string]string) string {
	if browserPlayable(streams["extern"]) {
		return "/" + camera + "_extern.ts"
	}
	return ""
}

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
	Camera string
	// URL is what the tile plays by itself: the sub stream where there is
	// one, because every tile on the page plays at once and sub is the
	// stream that makes that affordable.
	URL      string
	Playable bool
	// ExternURL is the balanced stream, empty unless this camera serves
	// one a browser can play. Pressing a tile steps up to it: same camera,
	// a picture worth looking at, and only one of them at a time.
	ExternURL string
}

// streamPrefix is where control.go mounts the streaming handler on this
// listener. It is the tile URL default: same origin, no CORS needed. See
// control.go's Handler for why that mount exists at all.
const streamPrefix = "/stream"

// tileURL builds the URL a tile plays. An empty StreamBase means "use this
// page's own same-origin mount" (see streamPrefix), not "same host as the
// browser": the streaming listener is on a different port, which is a
// different origin, and that listener is not allowed to grow CORS headers.
// A caller who sets StreamBase deliberately, to point tiles at some other
// reachable base, is trusted to have a reason and gets that base verbatim.
func tileURL(base, path string) string {
	if base == "" {
		return streamPrefix + path
	}
	return base + path
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
		t := tile{
			Camera:   cam,
			URL:      tileURL(s.opts.StreamBase, path),
			Playable: ok,
		}
		// Only when it is a step up: if the tile is already playing the
		// balanced stream because there is no sub, pressing it has nothing
		// better to switch to.
		if ext := externStream(cam, byCam[cam]); ext != "" && ok && !strings.HasSuffix(path, "_extern.ts") {
			t.ExternURL = tileURL(s.opts.StreamBase, ext)
		}
		out = append(out, t)
	}
	return out
}
