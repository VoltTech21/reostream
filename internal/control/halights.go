package control

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/mqtt"
)

// The floodlight, the siren and siren on detection over MQTT, and so in
// Home Assistant as a light, a button and a switch.
//
// The light here is the light now: ON lights it and OFF puts it out, both
// with message 288 over the media port, which leaves what the light does by
// itself (on motion, on a schedule) as it was. That is what a light entity
// means, and it is the same on every camera, web API or not; the CGI write
// on the settings page sets the mode as well, and its "off" would stop the
// light coming on for motion. The siren is 263, siren on detection the
// AudioTask block, as on the page.

// haCommandTopic is where Home Assistant (or anything) sends a command for
// one of a camera's controls.
func haCommandTopic(prefix, camera, object string) string {
	return prefix + "/" + camera + "/" + object + "/set"
}

// controlsEvery is how often learnControls looks again for cameras whose
// Support block has since been read (a battery camera's is read only when
// someone opens it, never to announce it).
var controlsEvery = 5 * time.Minute

// controlKinds is what cam's Support block says it can be driven to do.
func (s *Server) controlKinds(ctx context.Context, cam Camera) []string {
	sup := s.identForView(ctx, cam).Support
	if sup == nil {
		return nil
	}
	var out []string
	if sup.HasFloodlight(0) {
		out = append(out, "light")
	}
	if sup.HasSiren() {
		out = append(out, "siren", "siren_on_detection")
	}
	return out
}

// learnControls announces each camera's controls once its Support block
// says it has them, and publishes a wired camera's state for them. A
// battery camera is never woken for this: its block is the one read last,
// and its state is published when it is next commanded.
func (s *Server) learnControls(ctx context.Context, mq *mqtt.Client, prefix, version string, cams []config.Camera) {
	stated := map[string]bool{}
	for {
		for _, c := range cams {
			if ctx.Err() != nil {
				return
			}
			cam, err := s.byName(c.Name)
			if err != nil {
				continue
			}
			for _, kind := range s.controlKinds(ctx, cam) {
				if m, ok := relay.learn(prefix, version, cam.Name, kind); ok {
					if err := mq.Publish(m.Topic, m.Payload, 0, true); err != nil {
						log.Printf("control: home assistant: %v", err)
					}
				}
				if cam.UID != "" || stated[cam.Name+"/"+kind] {
					continue
				}
				if s.publishControlState(ctx, mq, prefix, cam, kind) {
					stated[cam.Name+"/"+kind] = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(controlsEvery):
		}
	}
}

// publishControlState reads one control's state off the camera and
// publishes it, retained. The siren is a button and has none.
func (s *Server) publishControlState(ctx context.Context, mq *mqtt.Client, prefix string, cam Camera, kind string) bool {
	rctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch kind {
	case "light":
		l, err := s.readFloodlightBC(rctx, cam)
		if err != nil || !l.Known {
			return false
		}
		return mq.Publish(prefix+"/"+cam.Name+"/light", []byte(haOnOff(l.Lit)), 0, true) == nil
	case "siren_on_detection":
		on, err := s.sirenOnDetection(rctx, cam)
		if err != nil {
			return false
		}
		return mq.Publish(prefix+"/"+cam.Name+"/siren_on_detection", []byte(haOnOff(on)), 0, true) == nil
	}
	return true
}

func haOnOff(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

// listenCommands subscribes to every camera's command topics. Retained
// messages are skipped, so a command is acted on once, when it is sent,
// and never again on a reconnect.
func (s *Server) listenCommands(ctx context.Context, mq *mqtt.Client, ev config.EventsConfig, prefix string, cams []config.Camera) {
	route := map[string][2]string{}
	var topics []string
	for _, c := range cams {
		for _, kind := range haControlKinds {
			t := haCommandTopic(prefix, c.Name, kind)
			route[t] = [2]string{c.Name, kind}
			topics = append(topics, t)
		}
	}
	if len(topics) == 0 {
		return
	}
	go mqtt.Subscribe(ctx, mqtt.SubscribeOptions{
		Addr: ev.MQTT, ClientID: motionClientID(), User: ev.MQTTUser, Pass: ev.MQTTPassword,
		SkipRetained: true,
		OnError:      func(err error) { log.Printf("control: command topics: %v", err) },
	}, topics, func(topic string, payload []byte) {
		if r, ok := route[topic]; ok {
			go s.handleCommand(ctx, mq, prefix, r[0], r[1], strings.ToUpper(strings.TrimSpace(string(payload))))
		}
	})
}

// handleCommand carries out one command and publishes the state the camera
// then reports. A command the camera does not confirm publishes nothing,
// so Home Assistant goes on showing what was last known rather than what
// was asked for.
func (s *Server) handleCommand(ctx context.Context, mq *mqtt.Client, prefix, camera, kind, payload string) {
	cam, err := s.byName(camera)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch kind {
	case "light":
		if payload != "ON" && payload != "OFF" {
			log.Printf("control: %s: floodlight command %q is not ON or OFF", camera, payload)
			return
		}
		st, reported, err := s.setFloodlightBC(ctx, cam, payload == "ON")
		if err != nil {
			log.Printf("control: %s: floodlight %s: %v", camera, payload, err)
			return
		}
		if reported {
			mq.Publish(prefix+"/"+camera+"/light", []byte(haOnOff(st.On())), 0, true)
		}
	case "siren":
		if payload != "PRESS" {
			log.Printf("control: %s: siren command %q is not PRESS", camera, payload)
			return
		}
		if err := s.soundSiren(ctx, cam); err != nil {
			log.Printf("control: %s: siren: %v", camera, err)
			return
		}
		log.Printf("control: %s: siren sounded", camera)
	case "siren_on_detection":
		if payload != "ON" && payload != "OFF" {
			log.Printf("control: %s: siren on detection command %q is not ON or OFF", camera, payload)
			return
		}
		res, err := s.setSirenOnDetection(ctx, cam, payload == "ON")
		if err != nil || res.Outcome != "confirmed" {
			log.Printf("control: %s: siren on detection %s: %s %v", camera, payload, res.Outcome, err)
			return
		}
		mq.Publish(prefix+"/"+camera+"/siren_on_detection", []byte(payload), 0, true)
	}
}
