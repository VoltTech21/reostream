package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/alarms"
	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/mqtt"
)

// The alert relay: every wired camera's own detections, followed by
// internal/alarms, go out as MQTT (Home Assistant, Node-RED, the cam wall)
// and, for objects, as Frigate events on the camera of the same name. It
// runs while the config has an [events] section with a destination.

type relayEvent struct {
	Camera string    `json:"camera"`
	Kind   string    `json:"kind"`
	On     bool      `json:"on"`
	Time   time.Time `json:"time"`
	Note   string    `json:"note,omitempty"`
}

type eventRelay struct {
	mu     sync.Mutex
	key    string
	cancel context.CancelFunc
	recent []relayEvent

	// handle publishes one change while the relay runs; watched names the
	// cameras with a live alert connection; clips holds the motion a clip
	// upload turned on, per camera, until it times out. See clipArrived.
	handle  func(alarms.Change)
	watched map[string]bool
	clips   map[string]*time.Timer

	// wake hears motion reported on a camera's motion_topic, to keep a
	// battery camera awake for it; reported is the last state heard per
	// camera. See motionReported.
	wake     func(camera string, on bool)
	reported map[string]bool

	// mq is the running relay's publisher, closed by the next syncRelay
	// before it opens its own, so that the old connection's "offline" is
	// said before the new one's "online". discovered is every Home
	// Assistant discovery topic the relay has published, so the next one
	// can remove what it no longer announces. See hadiscovery.go.
	mq         *mqtt.Client
	discovered map[string]bool
}

// motionReported takes one message from a camera's motion_topic: "ON" or
// "OFF" (any case). Only a change is relayed and passed to wake, so the
// same state again, including the relay's own retained message when the
// topic is the one it publishes to, does nothing.
func (r *eventRelay) motionReported(camera string, payload []byte) {
	var on bool
	switch strings.ToUpper(strings.TrimSpace(string(payload))) {
	case "ON":
		on = true
	case "OFF":
	default:
		return
	}
	r.mu.Lock()
	h, wake := r.handle, r.wake
	was, seen := r.reported[camera]
	if h == nil || (seen && was == on) {
		r.mu.Unlock()
		return
	}
	if r.reported == nil {
		r.reported = map[string]bool{}
	}
	r.reported[camera] = on
	r.mu.Unlock()
	if wake != nil {
		wake(camera, on)
	}
	h(alarms.Change{Camera: camera, Kind: "motion", On: on, Time: time.Now()})
}

// motionTopics maps each motion_topic in cams to its camera.
func motionTopics(cams []config.Camera) map[string]string {
	out := map[string]string{}
	for _, c := range cams {
		if c.MotionTopic != "" {
			out[c.MotionTopic] = c.Name
		}
	}
	return out
}

// motionClientID is the motion subscriber's MQTT client id.
func motionClientID() string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("reostream-motion-%x", b)
}

// motionWaker is the part of the supervisor that wakes a battery camera
// for motion (supervisor.Supervisor.Motion).
type motionWaker interface {
	Motion(camera string, on bool) bool
}

// motionHooker is the part of the supervisor that reports a battery
// camera's motion from its waking (supervisor.Supervisor.SetMotionHook).
type motionHooker interface {
	SetMotionHook(func(camera string, on bool))
}

// batteryMotion relays motion seen from a battery camera waking on its
// own. The supervisor has already woken the camera's stream for it. It
// returns at once: the supervisor calls it from its watch loop, and a
// Frigate call can take seconds.
func (r *eventRelay) batteryMotion(camera string, on bool) {
	r.mu.Lock()
	h := r.handle
	r.mu.Unlock()
	if h != nil {
		go h(alarms.Change{Camera: camera, Kind: "motion", On: on, Time: time.Now()})
	}
}

// clipMotionHold is how long motion stays on after a camera's last
// uploaded file.
var clipMotionHold = 30 * time.Second

// clipArrived turns a file a camera uploaded into motion, for a camera the
// relay has no live alert connection to: a battery camera, which sleeps
// and cannot be held open, but uploads each motion clip on its own (the
// snapshot about 18 s after the motion, the clip after it). Further files
// keep motion on; it goes off clipMotionHold after the last one.
func (r *eventRelay) clipArrived(camera string) {
	r.mu.Lock()
	h := r.handle
	if h == nil || r.watched[camera] {
		r.mu.Unlock()
		return
	}
	if t, ok := r.clips[camera]; ok {
		t.Reset(clipMotionHold)
		r.mu.Unlock()
		return
	}
	if r.clips == nil {
		r.clips = map[string]*time.Timer{}
	}
	r.clips[camera] = time.AfterFunc(clipMotionHold, func() {
		r.mu.Lock()
		delete(r.clips, camera)
		r.mu.Unlock()
		h(alarms.Change{Camera: camera, Kind: "motion", On: false, Time: time.Now()})
	})
	r.mu.Unlock()
	h(alarms.Change{Camera: camera, Kind: "motion", On: true, Time: time.Now()})
}

var relay eventRelay

func (r *eventRelay) remember(e relayEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recent = append(r.recent, e)
	if len(r.recent) > 200 {
		r.recent = r.recent[len(r.recent)-200:]
	}
}

func (r *eventRelay) snapshot() []relayEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]relayEvent, len(r.recent))
	copy(out, r.recent)
	return out
}

// frigateLabel maps a camera's object type to Frigate's label for it.
func frigateLabel(kind string) string {
	switch kind {
	case "person", "face", "visitor":
		return "person"
	case "vehicle":
		return "car"
	case "animal":
		return "animal"
	case "package":
		return "package"
	}
	return kind
}

// syncRelay starts, restarts or stops the relay to match the config.
func (s *Server) syncRelay() {
	cfg, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		return
	}
	var ev config.EventsConfig
	if cfg.Events != nil {
		ev = *cfg.Events
	}
	var names []string
	for _, c := range cfg.Cameras {
		if c.UID == "" && c.Channel == 0 && len(c.Streams) > 0 {
			names = append(names, c.Name)
		}
	}
	topics := motionTopics(cfg.Cameras)
	prefix := strings.Trim(ev.Prefix, "/")
	if prefix == "" {
		prefix = "reostream"
	}
	var discovery []discoveryMessage
	if ev.MQTT != "" && ev.HADiscovery {
		discovery = haDiscoveryMessages(prefix, buildVersion(), cfg.Cameras)
	}
	key := ""
	if ev.MQTT != "" || ev.Frigate != "" {
		b, _ := json.Marshal(ev)
		t, _ := json.Marshal(topics)
		d, _ := json.Marshal(discovery)
		key = string(b) + "|" + strings.Join(names, ",") + "|" + string(t) + "|" + string(d)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if key == relay.key {
		return
	}
	if relay.cancel != nil {
		relay.cancel()
		relay.cancel = nil
	}
	// The old relay's connection is closed before this one's opens, so
	// that its "offline" reaches Home Assistant before this one's "online"
	// rather than after it.
	if relay.mq != nil {
		relay.mq.Shutdown()
		relay.mq = nil
	}
	var mq *mqtt.Client
	if ev.MQTT != "" {
		mq = mqtt.New(ev.MQTT, "reostream-events", ev.MQTTUser, ev.MQTTPassword)
		if len(discovery) > 0 {
			mq.SetPresence(haStatusTopic(prefix), "online", "offline")
		}
		// Entities the last relay announced and this one does not -- a
		// camera removed or renamed, or discovery turned off -- are
		// removed from Home Assistant. With no broker any more there is
		// nobody to tell, and they stay there, unavailable. One failure
		// ends the loop: this runs under relay.mu, and every further
		// attempt at a broker that is down costs another dial timeout.
		for _, t := range staleDiscovery(relay.discovered, discovery) {
			if mq.Publish(t, nil, 0, true) != nil {
				break
			}
		}
	}
	relay.mq = mq
	relay.discovered = map[string]bool{}
	for _, m := range discovery {
		relay.discovered[m.Topic] = true
	}
	relay.key = key
	relay.handle = nil
	if key == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay.cancel = cancel
	fr := &frigateEvents{base: strings.TrimRight(ev.Frigate, "/"), motion: ev.FrigateMotion, open: map[string]string{}}
	var pubMu sync.Mutex
	handle := func(c alarms.Change) {
		e := relayEvent{Camera: c.Camera, Kind: c.Kind, On: c.On, Time: c.Time}
		pubMu.Lock()
		defer pubMu.Unlock()
		var notes []string
		// A change that raced the relay being replaced is dropped: its
		// client has been shut down, and the new relay has its own.
		if ctx.Err() != nil {
			return
		}
		if mq != nil {
			state := "OFF"
			if c.On {
				state = "ON"
			}
			if err := mq.Publish(prefix+"/"+c.Camera+"/"+c.Kind, []byte(state), 0, true); err != nil {
				notes = append(notes, "mqtt: "+err.Error())
			}
			b, _ := json.Marshal(e)
			mq.Publish(prefix+"/"+c.Camera+"/event", b, 0, false)
		}
		if fr.base != "" {
			if note := fr.change(ctx, c); note != "" {
				notes = append(notes, note)
			}
		}
		e.Note = strings.Join(notes, "; ")
		relay.remember(e)
	}
	relay.handle = handle
	relay.reported = nil
	relay.wake = nil
	if w, ok := s.opts.Supervisor.(motionWaker); ok {
		relay.wake = func(camera string, on bool) { w.Motion(camera, on) }
	}
	if ev.MQTT != "" && len(topics) > 0 {
		list := make([]string, 0, len(topics))
		for t := range topics {
			list = append(list, t)
		}
		go mqtt.Subscribe(ctx, mqtt.SubscribeOptions{
			// Unique per process: the broker drops the older of two
			// connections sharing a client id, so two reostreams (or a
			// test instance beside the real one) would knock each other
			// off every few seconds.
			Addr: ev.MQTT, ClientID: motionClientID(), User: ev.MQTTUser, Pass: ev.MQTTPassword,
			OnError: func(err error) { log.Printf("control: motion topics: %v", err) },
		}, list, func(topic string, payload []byte) {
			if cam, ok := topics[topic]; ok {
				relay.motionReported(cam, payload)
			}
		})
		log.Printf("control: listening for motion on %d topics", len(list))
	}
	if len(discovery) > 0 {
		s.startDiscovery(ctx, mq, ev, prefix, cfg.Cameras, discovery)
	}
	relay.watched = map[string]bool{}
	for _, name := range names {
		relay.watched[name] = true
	}
	for _, name := range names {
		cam, err := s.byName(name)
		if err != nil {
			continue
		}
		go alarms.Watch(ctx, name, func(ctx context.Context) (*baichuan.Conn, error) { return s.dial(ctx, cam) }, handle)
	}
	log.Printf("control: relaying alerts from %d cameras (mqtt %q, frigate %q)", len(names), ev.MQTT, ev.Frigate)
}

// frigateEvents opens a Frigate event when the camera starts seeing an
// object and ends it when it stops.
type frigateEvents struct {
	base   string
	motion bool
	open   map[string]string // camera/kind -> Frigate event id
}

var frigateClient = &http.Client{Timeout: 8 * time.Second}

// frigatePortHint is for the address people copy from their browser:
// Frigate's web port 8971 wants a login and, over https, has a certificate
// made for a name, not an address. Its API port 5000 takes events as is.
const frigatePortHint = " (use Frigate's API port: http://<frigate-ip>:5000, published in its Docker ports, not the 8971 web address)"

// frigateFailed says why a Frigate call did not go through.
func frigateFailed(err error) string {
	msg := "frigate: " + err.Error()
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) || strings.Contains(msg, "x509:") || strings.Contains(msg, "server gave HTTP response to HTTPS client") {
		msg += frigatePortHint
	} else if strings.Contains(msg, "connection refused") {
		msg += " (nothing answers there: Frigate's port 5000 has to be published, 5000:5000 in its Docker ports, or set under Network in the Home Assistant add-on)"
	}
	return msg
}

func (f *frigateEvents) change(ctx context.Context, c alarms.Change) string {
	if c.Kind == "motion" && !f.motion {
		return ""
	}
	key := c.Camera + "/" + c.Kind
	if c.On {
		if _, ok := f.open[key]; ok {
			return ""
		}
		body, _ := json.Marshal(map[string]any{
			"sub_label": "reolink", "score": 1, "duration": nil, "include_recording": true,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/api/events/%s/%s/create", f.base, c.Camera, frigateLabel(c.Kind)), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := frigateClient.Do(req)
		if err != nil {
			return frigateFailed(err)
		}
		defer resp.Body.Close()
		var out struct {
			EventID string `json:"event_id"`
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode == http.StatusUnauthorized {
			return "frigate: 401 " + out.Message + frigatePortHint
		}
		if resp.StatusCode != http.StatusOK || out.EventID == "" {
			return fmt.Sprintf("frigate: %d %s", resp.StatusCode, out.Message)
		}
		f.open[key] = out.EventID
		return ""
	}
	id, ok := f.open[key]
	if !ok {
		return ""
	}
	delete(f.open, key)
	body, _ := json.Marshal(map[string]any{"end_time": float64(c.Time.UnixMilli()) / 1000})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, f.base+"/api/events/"+id+"/end", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := frigateClient.Do(req)
	if err != nil {
		return frigateFailed(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("frigate end: %d", resp.StatusCode)
	}
	return ""
}

// eventsPage is Settings > Events.
type eventsPage struct {
	Title  string
	Events config.EventsConfig
	Flash  *Flash
}

func (s *Server) serveEventsPage(w http.ResponseWriter, r *http.Request) {
	page := eventsPage{Title: "Events", Flash: s.takeFlash(r)}
	if cfg, err := config.LoadRaw(s.opts.ConfigPath); err == nil && cfg.Events != nil {
		page.Events = *cfg.Events
	}
	s.render(w, r, "events.html", page)
}

func (s *Server) serveRecentEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(relay.snapshot())
}

func (s *Server) saveEventsPage(w http.ResponseWriter, r *http.Request) {
	ev := config.EventsConfig{
		MQTT:          strings.TrimSpace(r.FormValue("mqtt")),
		MQTTUser:      strings.TrimSpace(r.FormValue("mqtt_user")),
		MQTTPassword:  r.FormValue("mqtt_password"),
		Prefix:        strings.Trim(strings.TrimSpace(r.FormValue("prefix")), "/"),
		Frigate:       strings.TrimRight(strings.TrimSpace(r.FormValue("frigate")), "/"),
		FrigateMotion: r.FormValue("frigate_motion") == "1",
		HADiscovery:   r.FormValue("ha_discovery") == "1",
	}
	if ev.MQTTPassword == "" {
		if cfg, err := config.LoadRaw(s.opts.ConfigPath); err == nil && cfg.Events != nil {
			ev.MQTTPassword = cfg.Events.MQTTPassword // the box is blank, not cleared
		}
	}
	if ev.Frigate != "" && !strings.HasPrefix(ev.Frigate, "http://") && !strings.HasPrefix(ev.Frigate, "https://") {
		ev.Frigate = "http://" + ev.Frigate
	}
	var b strings.Builder
	b.WriteString("[events]\n")
	for _, kv := range [][2]string{{"mqtt", ev.MQTT}, {"mqtt_user", ev.MQTTUser}, {"mqtt_password", ev.MQTTPassword}, {"prefix", ev.Prefix}, {"frigate", ev.Frigate}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s = %q\n", kv[0], kv[1])
		}
	}
	if ev.FrigateMotion {
		b.WriteString("frigate_motion = true\n")
	}
	if ev.HADiscovery {
		b.WriteString("ha_discovery = true\n")
	}
	text, err := loadRawConfig(s.opts.ConfigPath)
	if err == nil {
		_, err = s.writeAndApply(upsertSection(text, "events", b.String()))
	}
	if err != nil {
		s.setFlash(w, r, Flash{Outcome: "refused", Message: "Not saved: " + err.Error()})
	} else {
		s.syncRelay()
		s.setFlash(w, r, Flash{Outcome: "confirmed", Message: "Saved"})
	}
	http.Redirect(w, r, "/settings/events", http.StatusSeeOther)
}
