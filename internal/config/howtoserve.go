package config

import "strings"

// HowToServe says how to turn a camera's stream on, for a reader that asked
// for one reostream does not serve. The RTSP log line used to stop at what
// was served, and a TrackMix owner whose camera had no zoom streams set up
// was left to find the config key on his own (YseanY, 11-02). The HTTP .ts
// 404 says the same since 11-04. It lives here, beside the keys it names,
// so both servers say it the same way.
func HowToServe(stream string) string {
	where := "tick " + stream + " for this camera on reostream's Cameras page"
	if stream == "zoom" || stream == "zoom_sub" {
		where = "turn it on from the camera's Live tab in reostream (it offers the zoom streams on a camera with a zoom lens) or tick " + stream + " on the Cameras page"
	}
	return "To serve it, " + where + ", or add \"" + stream + "\" to the camera's streams and rtsp lists in config.toml"
}

// NotServedOrder is the order a "not served" reply lists a camera's streams
// in, main first, and Suffix is how each one's path ends.
var NotServedOrder = []struct{ Suffix, Stream string }{
	{"", "main"}, {"_sub", "sub"}, {"_extern", "extern"}, {"_zoom", "zoom"}, {"_zoom_sub", "zoom_sub"},
}

// ServedPath is one path a camera is set up to serve, and whether video has
// come for it yet.
type ServedPath struct {
	Path  string
	Video bool
}

// ServedList is the "this camera serves" list of a "not served" reply. The
// two servers used to disagree: the HTTP .ts 404 listed every stream turned
// on, the RTSP one only those that had sent a keyframe, so with cam-19's
// main slots held elsewhere the RTSP 404 named only /cam-19_sub and the
// HTTP one named /cam-19.ts too, which then sent nothing (11-04). Both now
// list what the camera is set up to serve and say which has no video yet,
// so a reader is not sent to turn on a stream that is already on.
func ServedList(paths []ServedPath) string {
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = p.Path
		if !p.Video {
			parts[i] += " (no video yet)"
		}
	}
	return strings.Join(parts, ", ")
}
