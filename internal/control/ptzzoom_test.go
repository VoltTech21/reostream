package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// Zoom, the diagonals and patrol go out as the same PtzControl document the
// arrows use, with the firmware's own command words (fake hardware, 10-30,
// bug 3: the firmware took them, reostream had no way to send them).
func TestPtzPadSendsZoomDiagonalsAndPatrol(t *testing.T) {
	for _, tc := range []struct {
		form url.Values
		want []string
	}{
		{url.Values{"command": {"zoomInc"}, "speed": {"20"}}, []string{"<command>zoomInc</command>", "<speed>20</speed>"}},
		{url.Values{"command": {"leftUp"}}, []string{"<command>leftUp</command>"}},
		{url.Values{"command": {"startPatrol"}}, []string{"<command>startPatrol</command>", "<patrolId>0</patrolId>"}},
		{url.Values{"command": {"stopPatrol"}, "patrol": {"2"}}, []string{"<command>stopPatrol</command>", "<patrolId>2</patrolId>"}},
	} {
		key := baichuan.AESKey(testProbeNonce, "")
		fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
		fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzControl, 200, "")...)
		cam := fakecam.New(t, fixture)
		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one"),
			Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
				return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
			}})
		ts := httptest.NewServer(s.Handler())
		resp, err := http.PostForm(ts.URL+"/cameras/one/ptz", tc.form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ts.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%v: got %d, want 204", tc.form, resp.StatusCode)
		}
		s.ptzMu.Lock()
		if h := s.ptzHeld["one"]; h != nil && h.conn != nil {
			h.conn.Close()
		}
		s.ptzMu.Unlock()
		// fakecam records what it reads on its own goroutine, done once
		// it sees the close.
		for end := time.Now().Add(2 * time.Second); cam.Closes() == 0 && time.Now().Before(end); {
			time.Sleep(5 * time.Millisecond)
		}
		body := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDPtzControl, key)
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%v: sent %q, missing %s", tc.form, body, w)
			}
		}
	}
}

// Zoom shows only on a camera that reports a zoom range, patrol only on one
// that can patrol.
func TestPtzCardShowsZoomAndPatrolOnlyWhenTheCameraHasThem(t *testing.T) {
	sup, err := baichuan.ParseSupport([]byte(`<body><Support><ptzMode>pt</ptzMode></Support></body>`))
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	for _, v := range []ptzView{{HasZoom: true}, {HasPatrol: true}, {}} {
		w := httptest.NewRecorder()
		s.render(w, httptest.NewRequest("GET", "/cameras/one", nil), "camera.html",
			cameraPage{Title: "one", Camera: Camera{Name: "one"}, Head: camHead{Tab: "live"}, Support: &sup, PTZ: v})
		body := w.Body.String()
		if !strings.Contains(body, `data-cmd="leftUp"`) {
			t.Errorf("%+v: no diagonal on the pad", v)
		}
		if got := strings.Contains(body, `data-cmd="zoomInc"`); got != v.HasZoom {
			t.Errorf("%+v: zoom shown = %v", v, got)
		}
		if got := strings.Contains(body, `data-cmd="startPatrol"`); got != v.HasPatrol {
			t.Errorf("%+v: patrol shown = %v", v, got)
		}
	}
}
