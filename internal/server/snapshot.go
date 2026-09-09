package server

import (
	"net/http"
	"strings"
)

// serveKeyframe answers GET /<cam>.keyframe (and the _sub and _extern
// forms, see lookup) with the most recent raw video
// keyframe for that camera: the elementary stream bytes straight off the
// wire (baichuan.Frame.Video), not muxed, not decoded, not a JPEG.
//
// This is deliberately not a thumbnail endpoint. Producing an actual JPEG
// would need a decoder in the serving path, and every prior bug this
// project exists to get away from lived in exactly that kind of layer: a
// decode step that occasionally corrupts, hangs or leaks on real camera
// input. So this hands back the codec's own bytes and says what they are
// through Content-Type and X-Reostream-Codec, rather than pretending to be
// an image format it is not. A caller that wants an actual picture needs
// its own decoder (ffmpeg, a browser's MediaSource, whatever); this
// endpoint exists so a health check or a curious operator can confirm a
// camera is producing keyframes at all without paying the cost Subscribe
// pays: a client slot held for the life of a connection, and a wait for
// the next keyframe boundary (see hub.PublishKey) before anything arrives.
//
// Content-Type uses the RTP payload registrations for the two codecs this
// project carries (RFC 6184 for H.264; H.265 has no equivalent RFC but
// "video/H265" is the de facto label everything else uses) rather than
// something generic like application/octet-stream, since a caller that
// does know how to read raw Annex B can dispatch on it.
func (s *Server) serveKeyframe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasSuffix(r.URL.Path, ".keyframe") {
		http.NotFound(w, r)
		return
	}
	h, ok := s.lookup(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".keyframe"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	codec, es := h.Keyframe()
	if es == nil {
		// No video keyframe has arrived yet, whether because the stream
		// just started or the camera is down. 503 rather than 404: the
		// stream name is real, it just has nothing to show right now.
		http.Error(w, "no keyframe recorded yet", http.StatusServiceUnavailable)
		return
	}

	contentType := "application/octet-stream"
	switch strings.ToLower(codec) {
	case "h264":
		contentType = "video/H264"
	case "h265":
		contentType = "video/H265"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Reostream-Codec", codec)
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Write(es)
}
