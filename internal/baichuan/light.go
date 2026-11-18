package baichuan

import (
	"context"
	"encoding/xml"
	"fmt"
	"time"
)

// The floodlight and the siren over Baichuan, for a camera with no web API
// (an original Lumus answers on 9000 alone) and for a battery camera, which
// has none either.
//
// Both documents are the Reolink app's own, captured between the app and a
// TrackMix (2026-10-26) and matching, byte for byte in length, what the app
// sends an original Lumus (GitHub #29): 104 bytes of extension and 177 of
// FloodlightManual. Message 288 lights or darkens the light now, without
// touching what it does on its own (FloodlightTask, 289/290); the camera
// answers 200 with no body and then pushes a floodlight report (291) with
// every channel's state. It also pushes one straight after every login,
// which is how the state is read without changing it.

// floodlightManualSeconds is the duration the app sends with every 288, on
// and off alike.
const floodlightManualSeconds = 180

// FloodlightStatus is one channel's line in a floodlight report (291). A
// TrackMix reports both its lenses, channels 0 and 1.
type FloodlightStatus struct {
	Channel    int `xml:"channel"`
	Status     int `xml:"status"`
	Brightness int `xml:"brightness"`
}

// On reports whether the light is lit.
func (f FloodlightStatus) On() bool { return f.Status != 0 }

// ParseFloodlightReport reads a 291 document.
func ParseFloodlightReport(doc []byte) ([]FloodlightStatus, error) {
	var v struct {
		List []FloodlightStatus `xml:"FloodlightStatusList>FloodlightStatus"`
	}
	if err := xml.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("baichuan: floodlight report: %w", err)
	}
	if len(v.List) == 0 {
		return nil, fmt.Errorf("baichuan: floodlight report names no channel")
	}
	return v.List, nil
}

// floodlightFor picks channel's line out of a report.
func floodlightFor(list []FloodlightStatus, channel int) (FloodlightStatus, bool) {
	for _, f := range list {
		if f.Channel == channel {
			return f, true
		}
	}
	return FloodlightStatus{}, false
}

// FloodlightManualXML is the 288 document: status 1 lights the light, 0
// puts it out.
func FloodlightManualXML(channel int, on bool) []byte {
	status := 0
	if on {
		status = 1
	}
	return []byte(fmt.Sprintf("%s\n<body>\n<FloodlightManual version=\"1.1\">\n<channelId>%d</channelId>\n<status>%d</status>\n<duration>%d</duration>\n</FloodlightManual>\n</body>\n",
		cameraXMLHeader, channel, status, floodlightManualSeconds))
}

// SirenXML is the 263 document the app sends for its siren button: the
// siren sound played once (playMode 0, playTimes 1). It runs about 3
// seconds on a Lumus and stops by itself; there is no stop to send.
func SirenXML(channel int) []byte {
	return []byte(fmt.Sprintf("%s\n<body>\n<audioPlayInfo version=\"1.1\">\n<channelId>%d</channelId>\n<playMode>0</playMode>\n<playDuration>10</playDuration>\n<playTimes>1</playTimes>\n<onOff>1</onOff>\n</audioPlayInfo>\n</body>\n",
		cameraXMLHeader, channel))
}

// CommandRefused is a 288 or 263 the camera answered with a status
// other than 200.
type CommandRefused struct {
	What   string
	Status int16
}

func (e *CommandRefused) Error() string {
	return fmt.Sprintf("baichuan: %s: %s", e.What, ExplainStatus(e.Status))
}

// FloodlightNow reads the light's state from the report the camera pushes
// after login, waiting up to wait for it. ok is false when no report came:
// not every model sends one, and a connection that has already read other
// replies may have passed it by, so call this first on a fresh connection.
func (c *Conn) FloodlightNow(ctx context.Context, wait time.Duration) (st FloodlightStatus, ok bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		m, err := awaitMessage(ctx, c, MsgIDFloodlightReport)
		if err != nil {
			if ctx.Err() != nil {
				return FloodlightStatus{}, false, nil
			}
			return FloodlightStatus{}, false, err
		}
		list, perr := ParseFloodlightReport(m.XML)
		if perr != nil {
			continue
		}
		if st, ok := floodlightFor(list, c.opts.Channel); ok {
			return st, true, nil
		}
	}
}

// SetFloodlight lights the light (on) or puts it out, with message 288, and
// waits up to settle for a floodlight report saying it did. reported is
// false when the camera accepted the command and sent no report for this
// channel in time; st is then zero and means nothing.
//
// A report already queued before the command's own answer (the one pushed
// at login) is the state before, not after, and is skipped.
func (c *Conn) SetFloodlight(ctx context.Context, on bool, settle time.Duration) (st FloodlightStatus, reported bool, err error) {
	if err := c.SetConfig(MsgIDFloodlightSet, FloodlightManualXML(c.opts.Channel, on)); err != nil {
		return FloodlightStatus{}, false, fmt.Errorf("baichuan: floodlight: %w", err)
	}
	_, status, err := awaitReply(ctx, c, MsgIDFloodlightSet)
	if err != nil {
		return FloodlightStatus{}, false, fmt.Errorf("baichuan: floodlight: awaiting the answer: %w", err)
	}
	if status != 200 {
		return FloodlightStatus{}, false, &CommandRefused{What: "floodlight", Status: status}
	}
	// A TrackMix sends two reports a millisecond apart, the second with
	// its other lens caught up, so the first one for this channel that
	// says what was asked is the answer; one that does not is kept as the
	// last word in case nothing better arrives.
	wctx, cancel := context.WithTimeout(ctx, settle)
	defer cancel()
	for {
		m, err := awaitMessage(wctx, c, MsgIDFloodlightReport)
		if err != nil {
			if wctx.Err() != nil {
				return st, reported, nil
			}
			return st, reported, err
		}
		list, perr := ParseFloodlightReport(m.XML)
		if perr != nil {
			continue
		}
		if got, ok := floodlightFor(list, c.opts.Channel); ok {
			st, reported = got, true
			if got.On() == on {
				return st, true, nil
			}
		}
	}
}

// SoundSiren plays the siren once (263) and returns when the camera has
// answered.
func (c *Conn) SoundSiren(ctx context.Context) error {
	if err := c.SetConfig(MsgIDPlayStopAudio, SirenXML(c.opts.Channel)); err != nil {
		return fmt.Errorf("baichuan: siren: %w", err)
	}
	_, status, err := awaitReply(ctx, c, MsgIDPlayStopAudio)
	if err != nil {
		return fmt.Errorf("baichuan: siren: awaiting the answer: %w", err)
	}
	if status != 200 {
		return &CommandRefused{What: "siren", Status: status}
	}
	return nil
}
