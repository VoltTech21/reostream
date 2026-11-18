package control

import (
	"context"
	"encoding/json"
	"log"
	"slices"
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

// haControls are the entities that drive the camera rather than report it:
// the floodlight, the siren and siren on detection. They follow from what
// the camera's Support block says it has, which the config does not know,
// so each is announced once that block has been read (learnControls) and
// kept like a learned kind from then on. Home Assistant sends the command
// to the state topic plus /set (a press, for the siren); see
// handleCommand.
var haControls = map[string]haEntity{
	"light":              {component: "light", object: "light", name: "Floodlight"},
	"siren":              {component: "button", object: "siren", name: "Sound siren"},
	"siren_on_detection": {component: "switch", object: "siren_on_detection", name: "Siren on detection"},
}

// haControlKinds is haControls in a fixed order.
var haControlKinds = []string{"light", "siren", "siren_on_detection"}

// haKind finds a learned or control kind.
func haKind(kind string) (haEntity, bool) {
	if e, ok := haLearned[kind]; ok {
		return e, true
	}
	e, ok := haControls[kind]
	return e, ok
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
// A face, an "other" object and a doorbell press are not here: see
// haLearned.
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

// haLearned are the entities a wired camera gets once it has reported the
// kind: a face, an object the camera calls "other", a doorbell press
// ("visitor"). Nothing in the config says which cameras detect faces or
// are doorbells, and these on every camera would be noise, so each is
// announced the first time the relay publishes its topic (fake hardware,
// 10-30, bug 5: the topics were published and Home Assistant never showed
// them). Home Assistant keeps the entity from then on, so a doorbell's
// Visitor can be automated on after its first press.
var haLearned = map[string]haEntity{
	"face":    {component: "binary_sensor", object: "face", name: "Face", deviceClass: "occupancy"},
	"other":   {component: "binary_sensor", object: "other", name: "Other object", deviceClass: "occupancy"},
	"visitor": {component: "binary_sensor", object: "visitor", name: "Visitor"},
}

// haLearns reports whether a camera can learn entities: one with an alert
// connection of its own, the only kind that reports these.
func haLearns(c config.Camera) bool { return c.UID == "" && c.Channel == 0 && len(c.Streams) > 0 }

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
		for _, e := range haEntities(c) {
			out = append(out, haEntityMessage(prefix, version, c.Name, e))
		}
	}
	return out
}

// haInitialStates is OFF for every binary sensor haDiscoveryMessages
// announces, retained like the relay's own state messages. Without it a
// new entity read "unknown" in Home Assistant until the camera's first
// event of that kind, which on a quiet camera could be days (11-13). A
// state left ON by a reostream that stopped mid-event is put right by the
// same message. Learned kinds are not here: they are announced because an
// event of that kind arrived, and its state comes with it.
func haInitialStates(prefix string, cams []config.Camera) []discoveryMessage {
	var out []discoveryMessage
	for _, c := range cams {
		for _, e := range haEntities(c) {
			if e.component == "binary_sensor" {
				out = append(out, discoveryMessage{Topic: prefix + "/" + c.Name + "/" + e.object, Payload: []byte("OFF")})
			}
		}
	}
	return out
}

// haLearnedMessages builds the documents for the learned kinds of each
// camera still in cams that can learn them. learned is camera -> kinds.
func haLearnedMessages(prefix, version string, cams []config.Camera, learned map[string]map[string]bool) []discoveryMessage {
	var out []discoveryMessage
	for _, c := range cams {
		for _, kind := range haControlKinds {
			if learned[c.Name][kind] {
				out = append(out, haEntityMessage(prefix, version, c.Name, haControls[kind]))
			}
		}
		if !haLearns(c) {
			continue
		}
		for _, kind := range []string{"face", "other", "visitor"} {
			if learned[c.Name][kind] {
				out = append(out, haEntityMessage(prefix, version, c.Name, haLearned[kind]))
			}
		}
	}
	return out
}

// haEntityMessage is one entity's discovery document.
func haEntityMessage(prefix, version, camera string, e haEntity) discoveryMessage {
	node := haNodeID(camera)
	doc := map[string]any{
		"name":                  e.name,
		"unique_id":             node + "_" + e.object,
		"state_topic":           prefix + "/" + camera + "/" + e.object,
		"availability_topic":    haStatusTopic(prefix),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device": map[string]any{
			"identifiers":  []string{node},
			"name":         camera,
			"manufacturer": "Reolink",
		},
		"origin": map[string]any{
			"name": "reostream",
			"sw":   version,
			"url":  "https://github.com/VoltTech21/reostream",
		},
	}
	if e.deviceClass != "" {
		doc["device_class"] = e.deviceClass
	}
	switch e.component {
	case "binary_sensor":
		doc["payload_on"], doc["payload_off"] = "ON", "OFF"
	case "light", "switch":
		doc["payload_on"], doc["payload_off"] = "ON", "OFF"
		doc["command_topic"] = haCommandTopic(prefix, camera, e.object)
	case "button":
		delete(doc, "state_topic")
		doc["command_topic"] = haCommandTopic(prefix, camera, e.object)
		doc["payload_press"] = "PRESS"
	}
	if e.unit != "" {
		doc["unit_of_measurement"] = e.unit
		doc["state_class"] = "measurement"
	}
	if e.diagnostic {
		doc["entity_category"] = "diagnostic"
	}
	b, _ := json.Marshal(doc)
	return discoveryMessage{
		Topic:   haDiscoveryPrefix + "/" + e.component + "/" + node + "/" + e.object + "/config",
		Payload: b,
	}
}

// learn records that camera reported kind and, the first time for a kind
// in haLearned, returns its discovery document to publish. The topic joins
// the relay's discovered set, so it is retracted with the rest.
func (r *eventRelay) learn(prefix, version, camera, kind string) (discoveryMessage, bool) {
	e, ok := haKind(kind)
	if !ok {
		return discoveryMessage{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.learned[camera][kind] {
		return discoveryMessage{}, false
	}
	if r.learned == nil {
		r.learned = map[string]map[string]bool{}
	}
	if r.learned[camera] == nil {
		r.learned[camera] = map[string]bool{}
	}
	r.learned[camera][kind] = true
	m := haEntityMessage(prefix, version, camera, e)
	if r.discovered == nil {
		r.discovered = map[string]bool{}
	}
	r.discovered[m.Topic] = true
	return m, true
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
// homeassistant/status), with the entities learned since, and the battery
// cameras' levels as they change.
func (s *Server) startDiscovery(ctx context.Context, mq *mqtt.Client, ev config.EventsConfig, prefix string, cams []config.Camera, msgs []discoveryMessage) {
	// The states go after the documents, so Home Assistant has the entity
	// when its first state arrives.
	go announce(ctx, mq, append(slices.Clone(msgs), haInitialStates(prefix, cams)...), 30*time.Second)
	// A learned entity's config is retained by the broker. Reading it back
	// is how a restarted relay knows the entity is there, to retract it
	// when the camera is removed or discovery is turned off.
	topics := []string{haDiscoveryPrefix + "/status"}
	learnedTopic := map[string][2]string{}
	for _, c := range cams {
		for kind, e := range haControls {
			m := haEntityMessage(prefix, "", c.Name, e)
			topics = append(topics, m.Topic)
			learnedTopic[m.Topic] = [2]string{c.Name, kind}
		}
		if !haLearns(c) {
			continue
		}
		for kind, e := range haLearned {
			m := haEntityMessage(prefix, "", c.Name, e)
			topics = append(topics, m.Topic)
			learnedTopic[m.Topic] = [2]string{c.Name, kind}
		}
	}
	version := buildVersion()
	go mqtt.Subscribe(ctx, mqtt.SubscribeOptions{
		Addr: ev.MQTT, ClientID: motionClientID(), User: ev.MQTTUser, Pass: ev.MQTTPassword,
		OnError: func(err error) { log.Printf("control: home assistant status: %v", err) },
	}, topics, func(topic string, payload []byte) {
		if ck, ok := learnedTopic[topic]; ok {
			if len(payload) > 0 && ctx.Err() == nil {
				relay.learn(prefix, version, ck[0], ck[1])
			}
			return
		}
		if strings.EqualFold(strings.TrimSpace(string(payload)), "online") {
			relay.mu.Lock()
			all := append(haDiscoveryMessages(prefix, version, cams), haLearnedMessages(prefix, version, cams, relay.learned)...)
			relay.mu.Unlock()
			go announce(ctx, mq, all, 30*time.Second)
		}
	})
	go s.learnControls(ctx, mq, prefix, version, cams)
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
