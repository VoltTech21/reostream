package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
)

type fixedStatus map[string]server.StreamStatus

func (f fixedStatus) StreamStats() map[string]server.StreamStatus { return f }

func TestDashboardShowsEveryStreamAndItsError(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		// A config, because an install with no config file at all is an
		// unclaimed one and every route on it goes to the claim screen
		// first. One camera, because a config with none is a first run and
		// the dashboard sends that to setup.
		ConfigPath: writeTestConfig(t, "driveway"),
		Status: fixedStatus{
			"driveway/main": {Connected: true, Streaming: true, FPS: 24.9},
			"gate/sub":      {Connected: false, LastError: "dial tcp: refused"},
		},
	})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	// A stream is named by its camera's card and its own name inside it
	// ("driveway", then "main"), not by the joined "driveway/main" key the
	// stats map uses: the page groups by camera now. Both cameras must
	// still appear, including the one the stats report but the config does
	// not name, and the error must still be on the page.
	for _, want := range []string{
		`data-camera="driveway"`, `data-camera="gate"`,
		">main<", ">sub<", "dial tcp: refused",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard does not mention %q\n%s", want, page)
		}
	}
}

// TestDashboardIsOneCardPerCamera is the regression test for the shape of
// this page: a camera pulling three streams is ONE card carrying all three,
// not three cards. It also pins the two things the page had no way to show
// before -- the camera's address, and the URL a recorder points at each
// stream.
func TestDashboardIsOneCardPerCamera(t *testing.T) {
	cfg := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n" +
		"\n[[camera]]\nname = \"lounge\"\naddress = \"192.0.2.29\"\nusername = \"admin\"\npassword = \"\"\n" +
		"streams = [\"main\", \"sub\", \"extern\"]\n"
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Status: fixedStatus{
			"lounge/main":   {Connected: true, Streaming: true, FPS: 20},
			"lounge/sub":    {Connected: true, Streaming: true, FPS: 20},
			"lounge/extern": {Connected: true, Streaming: true, FPS: 20},
		},
	})
	page := getBody(t, ts, "/")

	if n := strings.Count(page, `data-camera="lounge"`); n != 1 {
		t.Fatalf("a camera with three streams rendered %d cards, want 1\n%s", n, page)
	}
	if !strings.Contains(page, "192.0.2.29") {
		t.Errorf("the card does not show the camera's address\n%s", page)
	}
	// The host the page was reached on, not the config's wildcard listen
	// address: a URL built from 0.0.0.0 works from nowhere.
	host := hostOf(t, ts.URL)
	for _, want := range []string{
		"http://" + host + ":8560/lounge.ts",
		"http://" + host + ":8560/lounge_sub.ts",
		"http://" + host + ":8560/lounge_extern.ts",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the card does not show the stream URL %q\n%s", want, page)
		}
	}
}

// hostOf is the host part of a test server's URL, which is what the page
// sees in the Host header and builds its stream URLs from.
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// TestOSDEndpointReportsBothOverlayFlags pins what the status page's
// toggles are filled in from: the camera's own document, read through the
// same curated block the settings page writes through, with the block name
// that actually answered so the write can post it back.
func TestOSDEndpointReportsBothOverlayFlags(t *testing.T) {
	key := baichuan.AESKey(testProbeNonce, "")
	// The RLC-810A's own shape (testdata/livefixtures/osd2.xml): the
	// timestamp on, the camera name off.
	osdXML := testXMLHeader + `<body><OsdChannelName><channelId>0</channelId><name>lounge</name><enable>0</enable></OsdChannelName><OsdDatetime><channelId>0</channelId><enable>1</enable></OsdDatetime></body>`
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, pairGetID(t, "osd get"), 200, osdXML)...)
	cam := fakecam.New(t, fixture)

	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Dial: func(ctx context.Context, c control.Camera) (*baichuan.Conn, error) {
			return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
		},
	})

	var got struct {
		Block  string `json:"block"`
		Error  string `json:"error"`
		Fields map[string]struct {
			On          bool   `json:"on"`
			Value       string `json:"value"`
			Unavailable string `json:"unavailable"`
		} `json:"fields"`
	}
	if err := json.Unmarshal([]byte(getBody(t, ts, "/cameras/one/osd")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("the camera answered but the endpoint reports %q", got.Error)
	}
	if got.Block != "osd get" {
		t.Errorf("block is %q, want the block that answered", got.Block)
	}
	if f := got.Fields["OsdDatetime/enable"]; !f.On || f.Value != "1" {
		t.Errorf("the timestamp flag reads %+v, want on", f)
	}
	if f := got.Fields["OsdChannelName/enable"]; f.On || f.Value != "0" {
		t.Errorf("the camera name flag reads %+v, want off", f)
	}
}

// TestOSDReadFailureDoesNotBreakTheRow is the other half of keeping the
// status page free: a camera that cannot be reached answers this endpoint
// with a reason a person can read, on the 200 path so the page can tell it
// apart from an expired session, and the page itself renders that camera's
// card either way because it never asked the camera anything.
func TestOSDReadFailureDoesNotBreakTheRow(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one", "two"),
		Status:          fixedStatus{"one/main": {Connected: true, Streaming: true}},
		Dial: func(ctx context.Context, c control.Camera) (*baichuan.Conn, error) {
			return nil, errors.New("dial tcp 192.0.2.10:9000: connect: no route to host")
		},
	})

	body := getBody(t, ts, "/cameras/one/osd")
	var got struct {
		Error  string         `json:"error"`
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "no route to host") {
		t.Errorf("the failure does not say why: %q", body)
	}
	if len(got.Fields) != 0 {
		t.Errorf("a camera that did not answer still reported flags: %q", body)
	}

	page := getBody(t, ts, "/")
	for _, want := range []string{`data-camera="one"`, `data-camera="two"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the status page lost %q when the camera could not be read\n%s", want, page)
		}
	}
	// And the switches must be rendered unreadable rather than guessed at:
	// disabled until the fetch answers.
	if !strings.Contains(page, `type="checkbox" disabled`) {
		t.Error("the overlay toggles do not start disabled, so the page is drawing a state it has not read")
	}
}

// TestDashboardAutoplaysOnlyTheCheapStream replaces an earlier guard that
// forbade autoplay outright.
//
// That guard was written for a live bug: the page created and loaded a
// player for every tile the instant it opened, which was eight
// simultaneous decodes on an eight camera fleet. What made that expensive
// was the stream, not the autoplay. A tile plays the sub stream, 640x360
// at about a quarter of a megabit, where main is 4K at six, and eight
// tiles waiting behind a play button are eight cameras nobody is watching.
//
// So the cost is still bounded, by these three rules.
func TestDashboardAutoplaysOnlyTheCheapStream(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("H264", nil)
	main := hub.New(4)
	main.SetKeyframe("H264", nil)
	hubs := server.StaticHubs(map[string]*hub.Hub{"gate/sub": h, "gate/main": main})
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "gate"),
		Hubs:            hubs,
		Status: fixedStatus{
			"gate/sub":  {Connected: true, Streaming: true},
			"gate/main": {Connected: true, Streaming: true},
		},
	})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	// The tile that plays by itself must be the sub stream. A page that
	// autoplayed main would be the original bug wearing a new name.
	if !strings.Contains(page, `data-src="/stream/gate_sub.ts"`) {
		t.Errorf("the tile does not play the sub stream:\n%s", page)
	}
	if strings.Contains(page, `data-src="/stream/gate.ts"`) {
		t.Error("the tile plays the main stream, which is what made eight at once expensive")
	}
	if !strings.Contains(page, "data-autoplay") {
		t.Error("no tile is marked to play by itself")
	}

	if !strings.Contains(page, `<script src="/assets/dashboard.js"></script>`) {
		t.Fatal("dashboard does not load its script")
	}
	js, err := http.Get(ts.URL + "/assets/dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	defer js.Body.Close()
	script, _ := io.ReadAll(js.Body)
	code := string(script)

	// Stopping a tile must destroy its player. Pausing the element is not
	// enough: mpegts.js keeps pulling the stream over XHR while paused,
	// which leaves the daemon serving a tab nobody is watching.
	if !strings.Contains(code, "player.destroy()") {
		t.Error("stopping a tile must destroy the player, not just pause the element")
	}
	// One stepped-up tile at a time: pressing a tile stops whatever was
	// stepped up before it.
	if !strings.Contains(code, "stopActive();") {
		t.Error("pressing a tile does not stop the previously stepped-up one")
	}
}

// TestDashboardBitrateRendersAsMbps pins the presentation change: the
// dashboard shows bitrate to a person as Mbps with one decimal, while
// /api/status and /metrics (internal/server) keep reporting the raw
// bits-per-second value untouched, since a recorder and Prometheus both
// parse that field.
func TestDashboardBitrateRendersAsMbps(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "driveway"),
		Status: fixedStatus{
			"driveway/main": {Connected: true, Streaming: true, BitrateBps: 6231488},
			"driveway/sub":  {Connected: true, Streaming: true, BitrateBps: 412000},
		},
	})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	for _, want := range []string{"6.2", "0.4"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard does not show bitrate %q Mbps\n%s", want, page)
		}
	}
	if strings.Contains(page, "6231488") || strings.Contains(page, "412000") {
		t.Fatal("dashboard rendered the raw bits-per-second value instead of Mbps")
	}
}
