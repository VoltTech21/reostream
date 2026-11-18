package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/mqtt"
)

// noWebAPI is a camera with nothing listening for HTTP or HTTPS, as an
// original Lumus (GitHub #29): a real dial to a port that refuses.
func noWebAPI(t *testing.T) func(Camera) (*cgi.Client, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return func(Camera) (*cgi.Client, error) { return cgi.Dial(addr, "admin", "") }
}

func lightReport(status int) string {
	return testXMLHeader + fmt.Sprintf(`<body><FloodlightStatusList version="1.1"><FloodlightStatus><channel>0</channel><status>%d</status><brightness>85</brightness></FloodlightStatus></FloodlightStatusList></body>`, status)
}

// lumus is a fake camera that answers a 288 and then reports the light lit.
func lumus(t *testing.T, after int) *fakecam.Camera {
	key := baichuan.AESKey(testProbeNonce, "")
	f := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	f = append(f, statusReply(t, key, baichuan.MsgIDFloodlightReport, 200, lightReport(1-after))...)
	f = append(f, statusReply(t, key, baichuan.MsgIDFloodlightSet, 200, "")...)
	f = append(f, statusReply(t, key, baichuan.MsgIDFloodlightReport, 200, lightReport(after))...)
	return fakecam.New(t, f)
}

func dialTo(cam *fakecam.Camera) func(context.Context, Camera) (*baichuan.Conn, error) {
	return func(ctx context.Context, _ Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
}

// With no web API at all the floodlight used to answer "connecting to cam1:
// could not reach the camera's HTTP or HTTPS API" and stop. It now goes
// over the media port, and is confirmed by the camera's own report.
func TestFloodlightWithNoWebAPIGoesOverBaichuan(t *testing.T) {
	cam := lumus(t, 1)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: noWebAPI(t), Dial: dialTo(cam)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/cam1/floodlight", url.Values{"option": {"on"}})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303: %s", resp.StatusCode, raw)
	}
	if banner := takenFlash(t, s, resp); banner.Outcome != "confirmed" {
		t.Fatalf("banner %+v, want confirmed", banner)
	}
	sent := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDFloodlightSet, baichuan.AESKey(testProbeNonce, ""))
	if sent != string(baichuan.FloodlightManualXML(0, true)) {
		t.Fatalf("camera received:\n%s", sent)
	}
}

// A web API that answers and refuses is a wrong password, not a camera
// without one: it must not be quietly routed round over Baichuan.
func TestARefusingWebAPIIsNotRoutedRound(t *testing.T) {
	dialled := false
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: noCGIDial,
		Dial: func(context.Context, Camera) (*baichuan.Conn, error) {
			dialled = true
			return nil, fmt.Errorf("no")
		}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp := postForm(t, flashBrowser(t), ts.URL+"/cameras/cam1/floodlight", url.Values{"option": {"on"}})
	if resp.StatusCode != http.StatusBadGateway || dialled {
		t.Fatalf("status %d, dialled Baichuan %v; want 502 and no Baichuan", resp.StatusCode, dialled)
	}
}

// The lights page of a camera with no web API offers the light now, on and
// off, read from the report the camera pushes at login, with its own mode
// from the task block beside it.
func TestLightsPageWithNoWebAPIShowsTheLightNow(t *testing.T) {
	key := baichuan.AESKey(testProbeNonce, "")
	f := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	f = append(f, statusReply(t, key, baichuan.MsgIDFloodlightReport, 200, lightReport(1))...)
	f = append(f, statusReply(t, key, baichuan.MsgIDFloodlightTaskGet, 200,
		testXMLHeader+`<body><FloodlightTask version="1.1"><channel>0</channel><alarmMode>1</alarmMode><enable>1</enable><brightness_cur>85</brightness_cur></FloodlightTask></body>`)...)
	cam := fakecam.New(t, f)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: noWebAPI(t), Dial: dialTo(cam)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/cameras/cam1/settings/lights")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)
	for _, want := range []string{"Floodlight now", "By itself: motion.", `value="on" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("lights page lacks %q", want)
		}
	}
	if strings.Contains(body, `value="schedule"`) {
		t.Error("the media port cannot set the schedule mode, and the page offers it")
	}
	if strings.Contains(body, "could not connect") {
		t.Error("a camera with no web API is shown as a connection failure")
	}
}

// A Home Assistant light, button and switch carry command topics; the
// button has no state.
func TestControlEntitiesHaveCommandTopics(t *testing.T) {
	light := haEntityMessage("reostream", "v", "lumus", haControls["light"])
	siren := haEntityMessage("reostream", "v", "lumus", haControls["siren"])
	sw := haEntityMessage("reostream", "v", "lumus", haControls["siren_on_detection"])
	var l, b, w map[string]any
	json.Unmarshal(light.Payload, &l)
	json.Unmarshal(siren.Payload, &b)
	json.Unmarshal(sw.Payload, &w)
	if light.Topic != "homeassistant/light/reostream_lumus/light/config" || l["command_topic"] != "reostream/lumus/light/set" || l["state_topic"] != "reostream/lumus/light" {
		t.Errorf("light: %s %s", light.Topic, light.Payload)
	}
	if siren.Topic != "homeassistant/button/reostream_lumus/siren/config" || b["command_topic"] != "reostream/lumus/siren/set" || b["payload_press"] != "PRESS" || b["state_topic"] != nil {
		t.Errorf("siren: %s %s", siren.Topic, siren.Payload)
	}
	if sw.Topic != "homeassistant/switch/reostream_lumus/siren_on_detection/config" || w["command_topic"] != "reostream/lumus/siren_on_detection/set" || w["payload_on"] != "ON" {
		t.Errorf("switch: %s %s", sw.Topic, sw.Payload)
	}
}

// Learned controls stay announced, on a battery camera too (which has no
// alert connection, so learns no object kinds).
func TestLearnedControlsAreAnnouncedForEveryCamera(t *testing.T) {
	cams := []config.Camera{{Name: "argus", UID: "X"}}
	msgs := haLearnedMessages("reostream", "v", cams, map[string]map[string]bool{"argus": {"light": true, "siren": true}})
	if len(msgs) != 2 || !strings.Contains(msgs[0].Topic, "/light/") || !strings.Contains(msgs[1].Topic, "/siren/") {
		t.Fatalf("got %+v", msgs)
	}
}

// An MQTT ON is 288 over the media port even where a web API answers: a
// light entity is the light now, and the web API's write would set the mode.
func TestMQTTLightCommandGoesOverBaichuan(t *testing.T) {
	cam := lumus(t, 1)
	cgiUsed := false
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: func(Camera) (*cgi.Client, error) { cgiUsed = true; return nil, fmt.Errorf("no") },
		Dial:    dialTo(cam)})
	mq := mqtt.New("127.0.0.1:1", "t", "", "")
	defer mq.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.handleCommand(ctx, mq, "reostream", "cam1", "light", "ON")
	if cgiUsed {
		t.Error("the MQTT light went through the web API")
	}
	sent := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDFloodlightSet, baichuan.AESKey(testProbeNonce, ""))
	if !strings.Contains(sent, "<status>1</status>") {
		t.Fatalf("camera received:\n%s", sent)
	}
}

// The siren button is one 263, the app's own document.
func TestSirenButtonSoundsTheSirenOnce(t *testing.T) {
	key := baichuan.AESKey(testProbeNonce, "")
	f := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	f = append(f, statusReply(t, key, baichuan.MsgIDPlayStopAudio, 200, "")...)
	cam := fakecam.New(t, f)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: noCGIDial, Dial: dialTo(cam)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/cameras/cam1/siren/sound", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ OK bool }
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if !got.OK {
		t.Fatalf("status %d, not ok", resp.StatusCode)
	}
	if sent := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDPlayStopAudio, key); sent != string(baichuan.SirenXML(0)) {
		t.Fatalf("camera received:\n%s", sent)
	}
}
