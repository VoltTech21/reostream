package control_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
)

type fixedStatus map[string]server.StreamStatus

func (f fixedStatus) StreamStats() map[string]server.StreamStatus { return f }

func TestDashboardShowsEveryStreamAndItsError(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
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

	for _, want := range []string{"driveway/main", "gate/sub", "dial tcp: refused"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard does not mention %q", want)
		}
	}
}

// TestDashboardNeverAutoplays is the regression test for the live bug: the
// old page created and loaded an mpegts.js player for every tile the
// instant the page loaded, which meant eight simultaneous video decodes on
// an eight camera fleet. No player may be created outside of a click
// handler, and the <video> element itself must never carry autoplay.
func TestDashboardNeverAutoplays(t *testing.T) {
	h := hub.New(4)
	h.SetKeyframe("H264", nil)
	hubs := server.StaticHubs(map[string]*hub.Hub{"gate/sub": h})
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		Hubs:            hubs,
		Status: fixedStatus{
			"gate/sub": {Connected: true, Streaming: true},
		},
	})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	if strings.Contains(page, "autoplay") {
		t.Fatal("dashboard video element carries autoplay")
	}

	// The only script that runs unconditionally at load is the wiring loop
	// that attaches click handlers. It must never itself call
	// mpegts.createPlayer: that call may only happen from inside
	// startPlayer, invoked by a click.
	loopStart := strings.Index(page, "for (const container of document.querySelectorAll")
	if loopStart == -1 {
		t.Fatal("dashboard is missing the click-wiring loop")
	}
	if strings.Contains(page[loopStart:], "createPlayer") {
		t.Fatal("the on-load wiring loop creates a player itself instead of only on click")
	}
	if !strings.Contains(page, "addEventListener('click'") {
		t.Fatal("dashboard never wires a click handler to start a tile")
	}
	if !strings.Contains(page, "player.destroy()") {
		t.Fatal("stopping a tile must destroy the player, not just pause the element, or mpegts.js keeps pulling the stream over XHR")
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
