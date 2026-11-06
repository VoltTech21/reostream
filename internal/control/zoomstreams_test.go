package control

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

const twoLensDeviceInfo = testXMLHeader + `<body><DeviceInfo><type>ipc</type><typeInfo>IPC</typeInfo><channelNum>2</channelNum></DeviceInfo></body>`

// A camera whose login reply says it has a second lens is offered zoom and
// zoom_sub when it is added, whether or not a zoom picture could be read in
// time: a TrackMix added while its zoom could not be read got neither, and
// the only way to add them was editing the config (YseanY, 11-02).
func TestTheProbeOffersZoomStreamsForASecondLens(t *testing.T) {
	for _, tc := range []struct {
		deviceInfo string
		want       bool
	}{
		{twoLensDeviceInfo, true},
		{testProbeDeviceInfo, false},
	} {
		cam := fakecam.New(t, loginHandshake(testProbeNonce, tc.deviceInfo))
		ctx, cancel := context.WithTimeout(withoutServiceSwitch(context.Background()), 600*time.Millisecond)
		rep := probeCamera(ctx, cam.Addr(), "admin", "")
		cancel()
		var names []string
		for _, st := range rep.Streams {
			names = append(names, st.Name)
			if (st.Name == "zoom" || st.Name == "zoom_sub") && (st.Undetermined || st.Absent || st.Note == "") {
				t.Errorf("%s offered as %+v, want it pickable with a note", st.Name, st)
			}
		}
		if got := slices.Contains(names, "zoom") && slices.Contains(names, "zoom_sub"); got != tc.want {
			t.Errorf("streams %v: zoom and zoom_sub offered = %v, want %v", names, got, tc.want)
		}
	}
	// And the setup page's list shows the note for a zoom stream whose
	// picture was not read.
	rec := httptest.NewRecorder()
	writeProbeJSON(rec, CameraReport{Streams: []StreamReport{{Name: "zoom_sub", Note: "zoom lens, small"}}})
	if !strings.Contains(rec.Body.String(), `"detail":"zoom lens, small"`) {
		t.Errorf("probe JSON: %s", rec.Body)
	}
}

// The camera list's zoom toggles come from the login reply, so a TrackMix
// whose main stream slots are both taken still gets them.
func TestIdentKnowsTheZoomLensFromTheLoginReply(t *testing.T) {
	skipIdent = false
	t.Cleanup(func() { skipIdent = true })
	fc := fakecam.New(t, loginHandshake(testProbeNonce, twoLensDeviceInfo))
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "driveway"),
		Dial: func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			return baichuan.Dial(ctx, fc.Addr(), baichuan.Options{})
		}})
	cam := Camera{Name: "driveway", Address: "192.0.2.10"}
	t.Cleanup(func() {
		identMu.Lock()
		delete(identCache, cam.Name+"@"+cam.Address)
		identMu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if !s.identFor(ctx, cam).Zoom {
		t.Fatal("a camera reporting two channels has no zoom lens")
	}
	if got := s.availableStreams(context.Background(), config.Camera{Name: "driveway", Streams: []string{"main"}}); !slices.Contains(got, "zoom_sub") {
		t.Errorf("camera list offers %v", got)
	}
}

// A configured camera with a zoom lens and no zoom streams gets them from
// its Live tab, as asked: zoom_sub alone, or both. RTSP serves them too
// when the camera is served over RTSP. Nothing is added unasked.
func TestTheLiveTabTurnsZoomStreamsOn(t *testing.T) {
	// The header reads the (cached) ident below.
	skipIdent = false
	t.Cleanup(func() { skipIdent = true })
	for _, tc := range []struct {
		which      string
		wantStream []string
	}{
		{"sub", []string{"main", "sub", "zoom_sub"}},
		{"both", []string{"main", "sub", "zoom", "zoom_sub"}},
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		text := "listen = \"0.0.0.0:8560\"\n\n[rtsp]\nlisten = \"0.0.0.0:8561\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n" +
			"[[camera]]\nname = \"driveway\"\naddress = \"192.0.2.10\"\nusername = \"admin\"\npassword = \"$CAM_PW\"\nstreams = [\"main\", \"sub\"]\nrtsp = [\"main\", \"sub\"]\n"
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAM_PW", "x")
		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
		cam := Camera{Name: "driveway", Address: "192.0.2.10"}
		identMu.Lock()
		identCache[cam.Name+"@"+cam.Address] = camIdent{Zoom: true, at: time.Now()}
		identMu.Unlock()
		ts := httptest.NewServer(s.Handler())
		page := func() string {
			resp, err := http.Get(ts.URL + "/cameras/driveway")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return string(b)
		}
		if !strings.Contains(page(), `action="/cameras/driveway/zoom"`) {
			t.Fatalf("the Live tab does not offer the zoom streams")
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.PostForm(ts.URL+"/cameras/driveway/zoom", url.Values{"which": {tc.which}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: status %d", tc.which, resp.StatusCode)
		}
		cfg, err := config.LoadRaw(path)
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.Cameras[0]
		if !slices.Equal(got.Streams, tc.wantStream) || !slices.Equal(got.RTSP, tc.wantStream) {
			t.Errorf("%s: streams %v rtsp %v, want %v for both", tc.which, got.Streams, got.RTSP, tc.wantStream)
		}
		if got.Password != "$CAM_PW" {
			t.Errorf("%s: password written as %q", tc.which, got.Password)
		}
		if strings.Contains(page(), `action="/cameras/driveway/zoom"`) {
			t.Errorf("%s: still offered once a zoom stream is pulled", tc.which)
		}
		ts.Close()
		identMu.Lock()
		delete(identCache, cam.Name+"@"+cam.Address)
		identMu.Unlock()
	}
}
