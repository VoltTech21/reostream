package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
)

func discoveryByTopic(t *testing.T, msgs []discoveryMessage) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range msgs {
		var doc map[string]any
		if err := json.Unmarshal(m.Payload, &doc); err != nil {
			t.Fatalf("%s: payload is not JSON: %v", m.Topic, err)
		}
		out[m.Topic] = doc
	}
	return out
}

func TestDiscoveryForAWiredCamera(t *testing.T) {
	msgs := haDiscoveryMessages("reostream", "v0.2.5", []config.Camera{
		{Name: "front_door", Address: "192.0.2.10", Streams: []string{"main", "sub"}},
	})
	docs := discoveryByTopic(t, msgs)
	if len(docs) != 4 {
		t.Fatalf("got %d entities, want motion, person, vehicle and animal: %v", len(docs), msgs)
	}
	for _, kind := range []string{"motion", "person", "vehicle", "animal"} {
		topic := "homeassistant/binary_sensor/reostream_front_door/" + kind + "/config"
		doc, ok := docs[topic]
		if !ok {
			t.Fatalf("no %s", topic)
		}
		want := map[string]any{
			"unique_id":             "reostream_front_door_" + kind,
			"state_topic":           "reostream/front_door/" + kind,
			"payload_on":            "ON",
			"payload_off":           "OFF",
			"availability_topic":    "reostream/status",
			"payload_available":     "online",
			"payload_not_available": "offline",
		}
		for k, v := range want {
			if doc[k] != v {
				t.Errorf("%s: %s = %v, want %v", kind, k, doc[k], v)
			}
		}
		dev, _ := doc["device"].(map[string]any)
		if ids, _ := dev["identifiers"].([]any); len(ids) != 1 || ids[0] != "reostream_front_door" || dev["name"] != "front_door" {
			t.Errorf("%s: device %v, want one device per camera", kind, dev)
		}
	}
	if docs["homeassistant/binary_sensor/reostream_front_door/motion/config"]["device_class"] != "motion" {
		t.Error("motion is not device_class motion")
	}
	if docs["homeassistant/binary_sensor/reostream_front_door/person/config"]["device_class"] != "occupancy" {
		t.Error("person is not device_class occupancy")
	}
}

func TestDiscoveryForABatteryCamera(t *testing.T) {
	docs := discoveryByTopic(t, haDiscoveryMessages("cams", "", []config.Camera{
		{Name: "garden", UID: "95270000ABCDEFGH", Address: "192.0.2.20", Streams: []string{"main"}},
	}))
	if len(docs) != 2 {
		t.Fatalf("got %v, want motion and battery", docs)
	}
	if _, ok := docs["homeassistant/binary_sensor/reostream_garden/motion/config"]; !ok {
		t.Error("battery camera has no motion")
	}
	b := docs["homeassistant/sensor/reostream_garden/battery/config"]
	want := map[string]any{
		"state_topic":         "cams/garden/battery",
		"device_class":        "battery",
		"unit_of_measurement": "%",
		"state_class":         "measurement",
		"entity_category":     "diagnostic",
		"availability_topic":  "cams/status",
	}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("battery: %s = %v, want %v", k, b[k], v)
		}
	}
	if _, ok := b["payload_on"]; ok {
		t.Error("a sensor carries binary_sensor payloads")
	}
}

// A camera behind an NVR has no alert connection, so it has nothing to
// announce unless something else reports its motion.
func TestDiscoveryForARecorderChannel(t *testing.T) {
	if msgs := haDiscoveryMessages("reostream", "", []config.Camera{
		{Name: "ch2", Address: "192.0.2.30", Channel: 2, Streams: []string{"main"}},
	}); len(msgs) != 0 {
		t.Fatalf("an NVR channel announced %d entities with nothing reporting them", len(msgs))
	}
	docs := discoveryByTopic(t, haDiscoveryMessages("reostream", "", []config.Camera{
		{Name: "ch2", Address: "192.0.2.30", Channel: 2, Streams: []string{"main"}, MotionTopic: "frigate/ch2/motion"},
	}))
	if len(docs) != 1 || docs["homeassistant/binary_sensor/reostream_ch2/motion/config"] == nil {
		t.Fatalf("got %v, want motion only", docs)
	}
}

func TestHANodeIDIsTopicSafe(t *testing.T) {
	for in, want := range map[string]string{
		"front_door": "reostream_front_door",
		"Back-Yard2": "reostream_Back-Yard2",
		"a b/c+#":    "reostream_a_b_c__",
	} {
		if got := haNodeID(in); got != want {
			t.Errorf("haNodeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStaleDiscovery(t *testing.T) {
	was := map[string]bool{"a": true, "b": true}
	got := staleDiscovery(was, []discoveryMessage{{Topic: "b"}, {Topic: "c"}})
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v, want [a]", got)
	}
	if got := staleDiscovery(was, nil); len(got) != 2 {
		t.Fatalf("turning discovery off removes %v, want both", got)
	}
}

// retainedBroker is just enough of an MQTT broker to watch what the relay
// publishes: it accepts every connection, acknowledges CONNECT and
// SUBSCRIBE, answers pings, and records each PUBLISH in arrival order.
type retainedBroker struct {
	ln   net.Listener
	mu   sync.Mutex
	pubs []string // "topic=payload"
}

func newRetainedBroker(t *testing.T) *retainedBroker {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &retainedBroker{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *retainedBroker) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		typ, err := br.ReadByte()
		if err != nil {
			return
		}
		n, mult := 0, 1
		for {
			d, err := br.ReadByte()
			if err != nil {
				return
			}
			n += int(d&0x7f) * mult
			mult *= 128
			if d&0x80 == 0 {
				break
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		switch typ >> 4 {
		case 1: // CONNECT
			c.Write([]byte{0x20, 0x02, 0x00, 0x00})
		case 3: // PUBLISH
			tl := int(body[0])<<8 | int(body[1])
			rest := body[2+tl:]
			if (typ>>1)&3 > 0 {
				id := rest[:2]
				rest = rest[2:]
				c.Write([]byte{0x40, 0x02, id[0], id[1]})
			}
			b.mu.Lock()
			b.pubs = append(b.pubs, string(body[2:2+tl])+"="+string(rest))
			b.mu.Unlock()
		case 8: // SUBSCRIBE
			c.Write([]byte{0x90, 0x03, body[0], body[1], 0x00})
		case 12: // PINGREQ
			c.Write([]byte{0xd0, 0x00})
		case 14: // DISCONNECT
			return
		}
	}
}

func (b *retainedBroker) published() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.pubs...)
}

// waitFor polls until every want has been published, in any order.
func (b *retainedBroker) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := map[string]bool{}
		for _, p := range b.published() {
			got[p] = true
		}
		missing := 0
		for _, w := range want {
			if !got[w] {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			pubs := b.published()
			sort.Strings(pubs)
			t.Fatalf("waited for %v; broker got %v", want, pubs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeEventsConfig(t *testing.T, path, broker string, discovery bool, cams ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n")
	fmt.Fprintf(&b, "\n[events]\nmqtt = %q\nha_discovery = %v\n", broker, discovery)
	for i, name := range cams {
		// Behind an NVR, with motion from a topic: a camera that has an
		// entity to announce without the relay dialing anything.
		fmt.Fprintf(&b, "\n[[camera]]\nname = %q\naddress = \"192.0.2.%d\"\nchannel = 1\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\nmotion_topic = \"other/%s/motion\"\n", name, i+10, name)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRelayAnnouncesAndRetractsCameras runs the real relay against a
// broker it can see into: the cameras are announced and said to be online,
// a camera taken out of the config is removed from Home Assistant, and
// turning discovery off removes the rest.
func TestRelayAnnouncesAndRetractsCameras(t *testing.T) {
	broker := newRetainedBroker(t)
	addr := broker.ln.Addr().String()
	path := filepath.Join(t.TempDir(), "config.toml")
	writeEventsConfig(t, path, addr, true, "porch", "drive")

	s := newTestServer(t, noCamera(Options{ConfigPath: path, AllowNoPassword: true}))
	t.Cleanup(func() {
		// relay is the process's one relay; leave it stopped for the
		// next test.
		os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)
		s.syncRelay()
	})
	s.syncRelay()

	porch := "homeassistant/binary_sensor/reostream_porch/motion/config"
	drive := "homeassistant/binary_sensor/reostream_drive/motion/config"
	broker.waitFor(t, "reostream/status=online")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var seen int
		for _, p := range broker.published() {
			if strings.HasPrefix(p, porch+"={") || strings.HasPrefix(p, drive+"={") {
				seen++
			}
		}
		if seen >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cameras not announced: %v", broker.published())
		}
		time.Sleep(20 * time.Millisecond)
	}

	writeEventsConfig(t, path, addr, true, "porch")
	s.syncRelay()
	broker.waitFor(t, drive+"=", "reostream/status=offline")

	writeEventsConfig(t, path, addr, false, "porch")
	s.syncRelay()
	broker.waitFor(t, porch+"=")

	// Offline is said by the old connection before the new one is opened,
	// so the last word on the status topic is the running relay's.
	var last string
	for _, p := range broker.published() {
		if strings.HasPrefix(p, "reostream/status=") {
			last = p
		}
	}
	if last != "reostream/status=offline" {
		t.Fatalf("status ends %q after discovery was turned off, want offline", last)
	}
}
