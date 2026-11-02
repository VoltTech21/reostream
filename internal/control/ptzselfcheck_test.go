package control_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// During its pan/tilt self check a TrackMix answers every move 400 and
// reads testStat 1 (fake hardware, 10-30, bug 7). The pad says so, and the
// refusal is not taken for a stale connection: no second login, no second
// send.
func TestPtzRefusalDuringTheSelfCheckSaysSoAndDoesNotRedial(t *testing.T) {
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzControl, 400, "")...)
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzAutoTest, 200,
		testXMLHeader+`<body><PtzAutoTest version="1.1"><channelId>0</channelId><testStat>1</testStat></PtzAutoTest></body>`)...)
	cam := fakecam.New(t, fixture)
	var dials atomic.Int32
	s := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Dial: func(ctx context.Context, c control.Camera) (*baichuan.Conn, error) {
			dials.Add(1)
			return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
		},
	})
	resp, err := http.PostForm(s.URL+"/cameras/one/ptz", url.Values{"command": {"left"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "self check") || strings.Contains(string(body), "status 400") {
		t.Errorf("the refusal does not say the camera is in its self check: %s", body)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("%d logins for one refused move, want 1", n)
	}
}

// A 400 outside the self check (testStat 2) is not blamed on it.
func TestPtzRefusalOutsideTheSelfCheckDoesNotBlameIt(t *testing.T) {
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzControl, 400, "")...)
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDPtzAutoTest, 200,
		testXMLHeader+`<body><PtzAutoTest version="1.1"><channelId>0</channelId><testStat>2</testStat></PtzAutoTest></body>`)...)
	cam := fakecam.New(t, fixture)
	s := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Dial: func(ctx context.Context, c control.Camera) (*baichuan.Conn, error) {
			return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
		},
	})
	resp, err := http.PostForm(s.URL+"/cameras/one/ptz", url.Values{"command": {"left"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "not running its pan/tilt self check") {
		t.Errorf("got %d %s, want a refusal that says it is not the self check", resp.StatusCode, body)
	}
}
