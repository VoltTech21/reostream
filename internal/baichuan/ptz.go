package baichuan

import (
	"context"
	"encoding/xml"
	"fmt"
)

// PTZCommand is a direction or lens movement for MsgIDPtzControl.
type PTZCommand string

const (
	PTZLeft      PTZCommand = "left"
	PTZRight     PTZCommand = "right"
	PTZUp        PTZCommand = "up"
	PTZDown      PTZCommand = "down"
	PTZLeftUp    PTZCommand = "leftUp"
	PTZLeftDown  PTZCommand = "leftDown"
	PTZRightUp   PTZCommand = "rightUp"
	PTZRightDown PTZCommand = "rightDown"
	PTZStop      PTZCommand = "stop"
	PTZZoomIn    PTZCommand = "zoomInc"
	PTZZoomOut   PTZCommand = "zoomDec"
	PTZFocusNear PTZCommand = "focusDec"
	PTZFocusFar  PTZCommand = "focusInc"
)

// ptzControlBody is the document for message 18. A move runs until a stop
// arrives, so a caller that moves must always stop.
type ptzControlBody struct {
	XMLName    xml.Name `xml:"body"`
	PtzControl struct {
		Version   string `xml:"version,attr"`
		ChannelID int    `xml:"channelId"`
		Speed     int    `xml:"speed"`
		Command   string `xml:"command"`
	} `xml:"PtzControl"`
}

func ptzControlXML(channel int, cmd PTZCommand, speed int) ([]byte, error) {
	var b ptzControlBody
	b.PtzControl.Version = "1.1"
	b.PtzControl.ChannelID = channel
	b.PtzControl.Speed = speed
	b.PtzControl.Command = string(cmd)
	return marshalDoc(&b, "ptz control")
}

// PTZControl starts or stops a movement and reports the status the camera
// answered. speed is the camera's own scale; the E1 takes 1 to 64.
func PTZControl(ctx context.Context, conn *Conn, cmd PTZCommand, speed int) (int16, error) {
	body, err := ptzControlXML(conn.opts.Channel, cmd, speed)
	if err != nil {
		return 0, err
	}
	return WriteConfig(ctx, conn, MsgIDPtzControl, body)
}

// PTZPosition is the camera's own pan, tilt and zoom counters from message
// 433. zPos is -1 on a camera with no zoom.
type PTZPosition struct {
	Pan, Tilt, Zoom int
}

// ReadPTZPosition reads message 433.
func ReadPTZPosition(ctx context.Context, conn *Conn) (PTZPosition, error) {
	raw, status, err := ReadConfig(ctx, conn, MsgIDGetPtzCurPos)
	if err != nil {
		return PTZPosition{}, err
	}
	if status != 200 {
		return PTZPosition{}, fmt.Errorf("baichuan: ptz position: camera answered status %d", status)
	}
	return parsePTZPosition(raw)
}

func parsePTZPosition(raw []byte) (PTZPosition, error) {
	var doc struct {
		Pos *struct {
			Pan  int `xml:"pPos"`
			Tilt int `xml:"tPos"`
			Zoom int `xml:"zPos"`
		} `xml:"ptzCurPos"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return PTZPosition{}, fmt.Errorf("baichuan: parse ptz position: %w", err)
	}
	if doc.Pos == nil {
		return PTZPosition{}, fmt.Errorf("baichuan: ptz position reply has no ptzCurPos")
	}
	return PTZPosition{doc.Pos.Pan, doc.Pos.Tilt, doc.Pos.Zoom}, nil
}

// PTZPreset is one saved position.
type PTZPreset struct {
	ID   int
	Name string
}

// ReadPTZPresets lists the saved positions with message 190. Message 19, the
// write side, answers a read with 200 and an empty body.
func ReadPTZPresets(ctx context.Context, conn *Conn) ([]PTZPreset, error) {
	raw, status, err := ReadConfig(ctx, conn, MsgIDPtzPreset2)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("baichuan: ptz presets: camera answered status %d", status)
	}
	return parsePTZPresets(raw)
}

func parsePTZPresets(raw []byte) ([]PTZPreset, error) {
	var doc struct {
		Presets []struct {
			ID   int    `xml:"id"`
			Name string `xml:"name"`
		} `xml:"PtzPreset>presetList>preset"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("baichuan: parse ptz presets: %w", err)
	}
	out := make([]PTZPreset, 0, len(doc.Presets))
	for _, p := range doc.Presets {
		out = append(out, PTZPreset{ID: p.ID, Name: p.Name})
	}
	return out, nil
}

type ptzPresetBody struct {
	XMLName   xml.Name `xml:"body"`
	PtzPreset struct {
		Version    string `xml:"version,attr"`
		ChannelID  int    `xml:"channelId"`
		PresetList struct {
			Preset struct {
				ID      int    `xml:"id"`
				Command string `xml:"command"`
			} `xml:"preset"`
		} `xml:"presetList"`
	} `xml:"PtzPreset"`
}

func ptzGotoPresetXML(channel, id int) ([]byte, error) {
	var b ptzPresetBody
	b.PtzPreset.Version = "1.1"
	b.PtzPreset.ChannelID = channel
	b.PtzPreset.PresetList.Preset.ID = id
	b.PtzPreset.PresetList.Preset.Command = "toPos"
	return marshalDoc(&b, "ptz preset")
}

// PTZGotoPreset moves the camera to a saved position.
func PTZGotoPreset(ctx context.Context, conn *Conn, id int) (int16, error) {
	body, err := ptzGotoPresetXML(conn.opts.Channel, id)
	if err != nil {
		return 0, err
	}
	return WriteConfig(ctx, conn, MsgIDPtzPreset, body)
}
