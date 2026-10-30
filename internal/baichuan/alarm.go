package baichuan

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// Camera alarms: after message 31 (header only, documented in the
// protocol notes as "start motion alarm") the camera pushes message 33,
// AlarmEventList, each time its own detection state changes.

const (
	msgAlarmStart = 31
	msgAlarmEvent = 33
)

// AlarmEvent is one state the camera reported.
type AlarmEvent struct {
	Channel int
	// Status is the camera's own word for what it sees ("MD" for motion,
	// "none" when it stops), and AI the object types it named, if any.
	Status    string
	AI        []string
	Recording bool
	Raw       []byte
}

// statusWords splits Status, which the firmware builds as a comma list
// ("MD", "visitor", "MD,visitor", "IOAlarm", "none").
func (e AlarmEvent) statusWords() []string {
	var out []string
	for _, w := range strings.Split(strings.ToLower(e.Status), ",") {
		if w = strings.TrimSpace(w); w != "" && w != "none" {
			out = append(out, w)
		}
	}
	return out
}

// Motion reports whether the camera says something is moving. A doorbell's
// call button arrives as status "visitor"; that is a press, not motion.
func (e AlarmEvent) Motion() bool {
	for _, w := range e.statusWords() {
		if w != "visitor" {
			return true
		}
	}
	return false
}

// Visitor reports a doorbell's call button being pressed.
func (e AlarmEvent) Visitor() bool {
	for _, w := range e.statusWords() {
		if w == "visitor" {
			return true
		}
	}
	return false
}

// StartAlarms asks the camera to push its alarm events.
func (c *Conn) StartAlarms(ctx context.Context) error {
	h := Header{
		MsgID:     msgAlarmStart,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	if err := c.w.Write(h, nil); err != nil {
		return err
	}
	m, err := awaitMessage(ctx, c, msgAlarmStart)
	if err != nil {
		return err
	}
	if s := m.Header.Status(); s != 200 && s != 0 {
		return fmt.Errorf("baichuan: alarm start: camera answered status %d", s)
	}
	return nil
}

// ParseAlarmEvents reads one AlarmEventList.
func ParseAlarmEvents(doc []byte) ([]AlarmEvent, error) {
	var d struct {
		Events []struct {
			Channel   int    `xml:"channelId"`
			Status    string `xml:"status"`
			Recording int    `xml:"recording"`
			AIType    string `xml:"AItype"`
			AI        string `xml:"aiType"`
		} `xml:"AlarmEventList>AlarmEvent"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("baichuan: alarm events: %w", err)
	}
	var out []AlarmEvent
	for _, e := range d.Events {
		ev := AlarmEvent{Channel: e.Channel, Status: strings.TrimSpace(e.Status), Recording: e.Recording == 1, Raw: doc}
		for _, src := range []string{e.AIType, e.AI} {
			for _, t := range strings.Split(src, ",") {
				if t = strings.TrimSpace(t); t != "" && t != "none" {
					ev.AI = append(ev.AI, t)
				}
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

// IsAlarmEvent reports whether a message is an alarm push.
func IsAlarmEvent(m Message) bool { return m.Header.MsgID == msgAlarmEvent }
