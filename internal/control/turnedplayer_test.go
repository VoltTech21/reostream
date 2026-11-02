package control

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A turned picture (#15) turned the <video> and its own controls with it.
// The player now carries reostream's play/pause and sound buttons outside
// the turned element, shown by reostreamTurn, which drops the native
// controls; the live player does not send a turned stream to iPhone
// Safari's video-only fullscreen, which ignores the CSS turn; and the
// recordings player is told how to turn.
func TestATurnedPlayerHasItsOwnControls(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/one", nil), "camera.html",
		cameraPage{Title: "one", Camera: Camera{Name: "one"}, Head: camHead{Tab: "live"},
			Streams: []liveStream{{Name: "main", Codec: "H265", Playable: true, Rotate: 90}}})
	body := w.Body.String()
	for _, want := range []string{`data-rotate="90"`, `class="live-ctl" hidden`, `data-ctl="play"`, `data-ctl="sound"`} {
		if !strings.Contains(body, want) {
			t.Errorf("live page: no %s", want)
		}
	}

	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.Local)
	w = httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/one/recordings", nil), "recordings.html",
		recPage{Title: "one", Cam: "one", Days: []time.Time{day}, Day: day, Rotate: 90})
	if b := w.Body.String(); !strings.Contains(b, `id="rec-player" data-camera="one" data-rotate="90"`) || !strings.Contains(b, `data-ctl="play"`) {
		t.Error("recordings player: no rotation or controls")
	}

	asset := func(name string) string {
		b, err := fs.ReadFile(assetFS, "assets/"+name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if js := asset("snapshot.js"); !strings.Contains(js, "window.reostreamTurn") || !strings.Contains(js, "video.removeAttribute('controls')") {
		t.Error("snapshot.js: reostreamTurn does not drop the native controls")
	}
	if js := asset("live.js"); !strings.Contains(js, "video.webkitEnterFullscreen && !turned") || !strings.Contains(js, "reostreamTurn(box, video, deg)") {
		t.Error("live.js: a turned stream still goes to video-only fullscreen, or is not turned through reostreamTurn")
	}
	if js := asset("rec.js"); !strings.Contains(js, "reostreamTurn(box, video") {
		t.Error("rec.js: recordings are not turned")
	}
}
