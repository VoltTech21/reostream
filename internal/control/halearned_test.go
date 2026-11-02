package control

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/alarms"
)

func writeWiredEventsConfig(t *testing.T, path, broker string, cams ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n")
	fmt.Fprintf(&b, "\n[events]\nmqtt = %q\nha_discovery = true\n", broker)
	for i, name := range cams {
		fmt.Fprintf(&b, "\n[[camera]]\nname = %q\naddress = \"192.0.2.%d\"\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n", name, i+10)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A doorbell press, a face or an "other" object gets its Home Assistant
// entity the first time the camera reports it (fake hardware, 10-30, bug
// 5), before the state that turns it on; it stays announced through a
// config change and is retracted with its camera.
func TestRelayAnnouncesLearnedEntities(t *testing.T) {
	broker := newRetainedBroker(t)
	addr := broker.ln.Addr().String()
	path := filepath.Join(t.TempDir(), "config.toml")
	writeWiredEventsConfig(t, path, addr, "door", "yard")
	relay.mu.Lock()
	relay.learned = nil
	relay.mu.Unlock()

	s := newTestServer(t, noCamera(Options{ConfigPath: path, AllowNoPassword: true}))
	t.Cleanup(func() {
		os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)
		s.syncRelay()
		relay.mu.Lock()
		relay.learned = nil
		relay.mu.Unlock()
	})
	s.syncRelay()
	broker.waitFor(t, "reostream/status=online")

	visitor := "homeassistant/binary_sensor/reostream_door/visitor/config"
	for _, p := range broker.published() {
		if strings.Contains(p, "/visitor/") || strings.Contains(p, "/face/") || strings.Contains(p, "/other/") {
			t.Fatalf("announced before the camera reported it: %s", p)
		}
	}
	relay.mu.Lock()
	h := relay.handle
	relay.mu.Unlock()
	h(alarms.Change{Camera: "door", Kind: "visitor", On: true, Time: time.Now()})
	h(alarms.Change{Camera: "door", Kind: "visitor", On: false, Time: time.Now()})
	h(alarms.Change{Camera: "door", Kind: "visitor", On: true, Time: time.Now()})
	broker.waitFor(t, "reostream/door/visitor=ON")

	var docs, at, on int
	for i, p := range broker.published() {
		if strings.HasPrefix(p, visitor+"={") {
			docs++
			at = i
			var doc map[string]any
			json.Unmarshal([]byte(strings.TrimPrefix(p, visitor+"=")), &doc)
			if doc["state_topic"] != "reostream/door/visitor" || doc["name"] != "Visitor" || doc["payload_on"] != "ON" {
				t.Errorf("visitor document: %v", doc)
			}
			if _, ok := doc["device_class"]; ok {
				t.Errorf("visitor has a device_class: %v", doc)
			}
		}
		if p == "reostream/door/visitor=ON" && on == 0 {
			on = i
		}
	}
	if docs != 1 || at > on {
		t.Fatalf("visitor announced %d times, at %d, first ON at %d; want once, before it", docs, at, on)
	}

	// A config change that keeps the camera keeps the entity.
	writeWiredEventsConfig(t, path, addr, "door")
	s.syncRelay()
	broker.waitFor(t, "homeassistant/binary_sensor/reostream_yard/motion/config=")
	for _, p := range broker.published() {
		if p == visitor+"=" {
			t.Fatal("the learned entity was retracted though its camera stayed")
		}
	}
	// Removing the camera retracts it.
	writeWiredEventsConfig(t, path, addr, "yard")
	s.syncRelay()
	broker.waitFor(t, visitor+"=")
}
