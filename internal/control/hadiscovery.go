package control

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/mqtt"
)

// Home Assistant MQTT discovery: with [events] ha_discovery on, each camera
// the relay has alerts for appears in Home Assistant as a device by itself,
// with no YAML. The entities read the same topics the relay already
// publishes, so discovery adds documents describing them and nothing else:
// a retained config message per entity under homeassistant/, a presence
// topic Home Assistant reads as availability, and the battery level, which
// the relay did not publish before.

// haDiscoveryPrefix is Home Assistant's default discovery prefix. It is not
// configurable here; an install that changed Home Assistant's would be the
// first to ask.
const haDiscoveryPrefix = "homeassistant"

// discoveryMessage is one retained message to publish.
type discoveryMessage struct {
	Topic   string
	Payload []byte
}

// haEntity is one entity of a camera's device.
type haEntity struct {
	component   string // binary_sensor or sensor
	object      string // the last topic level, and the unique_id suffix
	name        string
	deviceClass string
	unit        string
	diagnostic  bool
}

// haEntities is what a camera can report, which follows from how the relay
// hears it. A wired camera on its own is watched over its own alert
// connection, which names motion and the three object types its AI reports
// most. A battery camera cannot be held open, so the relay has only its
// motion, from its waking and its uploads, and its battery level from the
// UDP search. A camera behind an NVR or Home Hub has no alert connection at
// all, and so nothing, unless another tool reports its motion on a
// motion_topic.
//
// The camera's doorbell press is not here yet. The relay publishes it as
// "visitor", but nothing here knows which cameras are doorbells, and a
// Visitor sensor on every camera would be noise.
func haEntities(c config.Camera) []haEntity {
	motion := haEntity{component: "binary_sensor", object: "motion", name: "Motion", deviceClass: "motion"}
	var out []haEntity
	switch {
	case c.UID != "":
		out = append(out, motion, haEntity{component: "sensor", object: "battery", name: "Battery",
			deviceClass: "battery", unit: "%", diagnostic: true})
	case c.Channel == 0 && len(c.Streams) > 0:
		out = append(out, motion,
			haEntity{component: "binary_sensor", object: "person", name: "Person", deviceClass: "occupancy"},
			haEntity{component: "binary_sensor", object: "vehicle", name: "Vehicle", deviceClass: "occupancy"},
			haEntity{component: "binary_sensor", object: "animal", name: "Animal", deviceClass: "occupancy"})
	case c.MotionTopic != "":
		out = append(out, motion)
	}
	return out
}

// haNodeID makes a camera name safe as a discovery topic level and a
// unique_id, which Home Assistant limits to letters, digits, "_" and "-".
// Camera names are slugs already; this is for one written by hand.
func haNodeID(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "reostream_" + b.String()
}

// haStatusTopic is where the relay's MQTT connection says online or
// offline; see mqtt.Client.SetPresence.
func haStatusTopic(prefix string) string { return prefix + "/status" }

// haDiscoveryMessages builds every camera's discovery documents. prefix is
// the relay's topic prefix, which the state topics live under.
func haDiscoveryMessages(prefix, version string, cams []config.Camera) []discoveryMessage {
	var out []discoveryMessage
	for _, c := range cams {
		node := haNodeID(c.Name)
		device := map[string]any{
			"identifiers":  []string{node},
			"name":         c.Name,
			"manufacturer": "Reolink",
		}
		for _, e := range haEntities(c) {
			doc := map[string]any{
				"name":                  e.name,
				"unique_id":             node + "_" + e.object,
				"state_topic":           prefix + "/" + c.Name + "/" + e.object,
				"availability_topic":    haStatusTopic(prefix),
				"payload_available":     "online",
				"payload_not_available": "offline",
				"device_class":          e.deviceClass,
				"device":                device,
				"origin": map[string]any{
					"name": "reostream",
					"sw":   version,
					"url":  "https://github.com/VoltTech21/reostream",
				},
			}
			if e.component == "binary_sensor" {
				doc["payload_on"], doc["payload_off"] = "ON", "OFF"
			}
			if e.unit != "" {
				doc["unit_of_measurement"] = e.unit
				doc["state_class"] = "measurement"
			}
			if e.diagnostic {
				doc["entity_category"] = "diagnostic"
			}
			b, _ := json.Marshal(doc)
			out = append(out, discoveryMessage{
				Topic:   haDiscoveryPrefix + "/" + e.component + "/" + node + "/" + e.object + "/config",
				Payload: b,
			})
		}
	}
	return out
}

// staleDiscovery lists the topics in was that are not in now: entities of a
// camera since removed or renamed, or every entity once discovery is turned
// off. Publishing an empty retained message to each is how Home Assistant
// is told to remove it.
func staleDiscovery(was map[string]bool, now []discoveryMessage) []string {
	keep := map[string]bool{}
	for _, m := range now {
		keep[m.Topic] = true
	}
	var out []string
	for t := range was {
		if !keep[t] {
			out = append(out, t)
		}
	}
	return out
}

// announce publishes msgs, retrying every retry until the broker has taken
// all of them or ctx ends: a broker that is down when the relay starts
// would otherwise leave Home Assistant without the devices until the next
// config change.
func announce(ctx context.Context, mq *mqtt.Client, msgs []discoveryMessage, retry time.Duration) {
	for {
		failed := 0
		for _, m := range msgs {
			if ctx.Err() != nil {
				return
			}
			if err := mq.Publish(m.Topic, m.Payload, 0, true); err != nil {
				failed++
			}
		}
		if failed == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// startDiscovery announces the cameras, then keeps them announced: again
// whenever Home Assistant says it has started (it publishes "online" to
// homeassistant/status), and the battery cameras' levels as they change.
func (s *Server) startDiscovery(ctx context.Context, mq *mqtt.Client, ev config.EventsConfig, prefix string, cams []config.Camera, msgs []discoveryMessage) {
	go announce(ctx, mq, msgs, 30*time.Second)
	go mqtt.Subscribe(ctx, mqtt.SubscribeOptions{
		Addr: ev.MQTT, ClientID: motionClientID(), User: ev.MQTTUser, Pass: ev.MQTTPassword,
		OnError: func(err error) { log.Printf("control: home assistant status: %v", err) },
	}, []string{haDiscoveryPrefix + "/status"}, func(_ string, payload []byte) {
		if strings.EqualFold(strings.TrimSpace(string(payload)), "online") {
			go announce(ctx, mq, msgs, 30*time.Second)
		}
	})
	var battery []config.Camera
	for _, c := range cams {
		if c.UID != "" && c.Address != "" {
			battery = append(battery, c)
		}
	}
	if len(battery) > 0 {
		go s.publishBatteries(ctx, mq, prefix, battery)
	}
	log.Printf("control: announcing %d entities to home assistant", len(msgs))
}

// publishBatteries publishes each battery camera's level, retained, when it
// changes. The level comes from the same cache the status page reads, which
// asks the camera with a UDP search that does not wake it; the first ask
// answers -1 and fills in behind, so the loop starts quickly and then
// settles to the cache's own pace.
func (s *Server) publishBatteries(ctx context.Context, mq *mqtt.Client, prefix string, cams []config.Camera) {
	last := map[string]int{}
	wait := 5 * time.Second
	for {
		for _, c := range cams {
			lvl := s.batteries.level(c.Name, c.Address)
			if was, ok := last[c.Name]; lvl < 0 || (ok && was == lvl) {
				continue
			}
			if mq.Publish(prefix+"/"+c.Name+"/battery", []byte(strconv.Itoa(lvl)), 0, true) == nil {
				last[c.Name] = lvl
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = batteryEvery
	}
}
