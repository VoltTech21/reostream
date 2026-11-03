package control

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// The add-camera form's box decides whether the probe may turn the camera's
// services on: a tester objected to reostream writing camera settings
// unasked. Ticked (the default) and a caller that sends no box at all keep
// the old behavior; unticked, the probe does not even read them.
func TestProbeTurnsServicesOnOnlyWhenTheBoxIsTicked(t *testing.T) {
	for _, tc := range []struct {
		form url.Values
		want bool
	}{
		{url.Values{"services": {"0", "1"}}, true},
		{url.Values{"services": {"0"}}, false},
		{url.Values{}, true},
	} {
		var allowed *bool
		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t),
			Probe: func(ctx context.Context, addr, user, pass string) CameraReport {
				v := serviceSwitchAllowed(ctx)
				allowed = &v
				return CameraReport{Err: "stop here"}
			}})
		ts := httptest.NewServer(s.Handler())
		form := url.Values{"address": {"192.0.2.10"}}
		for k, v := range tc.form {
			form[k] = v
		}
		resp, err := http.PostForm(ts.URL+"/setup/probe", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ts.Close()
		if allowed == nil || *allowed != tc.want {
			t.Errorf("%v: services allowed = %v, want %v", tc.form, allowed, tc.want)
		}
	}

	// And probeCamera honors it: with the box unticked no netport read
	// (message 37, the first half of turning services on) goes out.
	for _, allow := range []bool{true, false} {
		cam := fakecam.New(t, loginHandshake(testProbeNonce, testProbeDeviceInfo))
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		if !allow {
			ctx = withoutServiceSwitch(ctx)
		}
		probeCamera(ctx, cam.Addr(), "admin", "")
		cancel()
		// fakecam records what it reads on its own goroutine.
		for end := time.Now().Add(time.Second); cam.Closes() == 0 && time.Now().Before(end); {
			time.Sleep(10 * time.Millisecond)
		}
		if got := sentMessage(cam.Received(), baichuan.MsgIDGetNetPort); got != allow {
			t.Errorf("allowed %v: netport read sent = %v", allow, got)
		}
	}
}

// sentMessage reports whether a message with id went out to the camera.
func sentMessage(received []byte, id uint32) bool {
	for off := 0; off+20 <= len(received); {
		h, n, err := baichuan.DecodeHeader(received[off:])
		if err != nil {
			return false
		}
		if h.MsgID == id {
			return true
		}
		off += n + int(h.MsgLen)
	}
	return false
}

// The form carries the box, ticked.
func TestAddCameraFormHasTheServicesBoxTicked(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `name="services" value="1" checked`) {
		t.Error("no ticked services box on the add-camera form")
	}
}

// The network scan only looks: a tester asked that reostream not change
// camera settings unasked, and the scan runs on its own when the setup
// page opens. The services are switched when cameras are added from it.
func TestTheScanNeverTurnsServicesOn(t *testing.T) {
	port := listenLoopback(t, "127.0.0.4")
	allowed := make(chan bool, 8)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeDiscoverConfig(t, "elsewhere", "192.0.2.99"),
		Probe: func(ctx context.Context, addr, user, pass string) CameraReport {
			allowed <- serviceSwitchAllowed(ctx)
			return CameraReport{Err: "stop here"}
		}})
	s.sweepPort = port
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for _, path := range []string{"/setup/discover", "/setup/identify"} {
		form := url.Values{"range": {loopbackRange}, "address": {"127.0.0.4"}, "services": {"0", "1"}}
		resp, err := http.PostForm(ts.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		select {
		case v := <-allowed:
			if v {
				t.Errorf("%s: the scan's probe may turn services on", path)
			}
		default:
			t.Errorf("%s: no probe ran", path)
		}
	}
}

// The scan's add form carries the same box, ticked.
func TestTheScanAddFormHasTheServicesBoxTicked(t *testing.T) {
	port := listenLoopback(t, "127.0.0.5")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeDiscoverConfig(t, "elsewhere", "192.0.2.99"),
		Probe: stubReport("IPC, 2560*1920, 1 channels")})
	s.sweepPort = port
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.PostForm(ts.URL+"/setup/discover", url.Values{"range": {loopbackRange}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `name="services" value="1" checked`) {
		t.Errorf("no ticked services box on the scan's add form:\n%s", b)
	}
}

// A battery camera on a network the scan does not reach (another VLAN)
// can still be added by its UID, through the scan's own add.
func TestSetupPageOffersABatteryCameraByUID(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`id="battery-form"`, `name="uid"`, `/setup/add-found`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("setup page is missing %s", want)
		}
	}
}
