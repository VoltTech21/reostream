package control

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

func TestListenersChangedNamesEveryPortThatNeedsARestart(t *testing.T) {
	old := &config.Config{
		Listen:  "0.0.0.0:8560",
		RTSP:    &config.RTSPConfig{Listen: "0.0.0.0:8561"},
		Control: &config.ControlConfig{Listen: "0.0.0.0:8562"},
	}
	next := &config.Config{
		Listen:  "0.0.0.0:9560",
		RTSP:    &config.RTSPConfig{Listen: "0.0.0.0:8561"},
		Control: &config.ControlConfig{Listen: "0.0.0.0:9562"},
	}
	got := listenersChanged(old, next)
	if len(got) != 2 {
		t.Fatalf("got %v, want the http and control listeners", got)
	}
}

func TestListenersChangedIsEmptyWhenOnlyCamerasMoved(t *testing.T) {
	old := &config.Config{
		Listen:  "0.0.0.0:8560",
		Cameras: []config.Camera{{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}}},
	}
	next := &config.Config{
		Listen:  "0.0.0.0:8560",
		Cameras: []config.Camera{{Name: "a", Address: "2.2.2.2", Streams: []string{"main"}}},
	}
	if got := listenersChanged(old, next); len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}
