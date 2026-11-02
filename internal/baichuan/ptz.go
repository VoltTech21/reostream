package baichuan

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PtzRefused is a PTZ command the camera answered with a status other than
// 200: the connection is fine, the camera said no. A TrackMix says 400 to
// every move while its pan/tilt self check runs, about a minute after it
// starts and during a calibrate.
type PtzRefused struct {
	What   string
	Status int
}

func (e *PtzRefused) Error() string {
	return fmt.Sprintf("baichuan: %s: camera answered status %d", e.What, e.Status)
}

// PtzControl moves the camera: command is left, right, up, down, a
// diagonal (leftUp, leftDown, rightUp, rightDown), zoomInc, zoomDec or stop.
// The document is the one the Reolink desktop client sends (captured
// 2026-10-22: speed 34 for a move, speed 0 with stop). A move or a zoom
// continues until a stop arrives, so a caller that starts one must send the
// stop. The diagonals and zoom are the firmware's own command words
// (libnetpublic's _get_ptz_cmd); the fake TrackMix moved both motors for
// leftUp and zoomed for zoomInc (10-30).
func (c *Conn) PtzControl(ctx context.Context, command string, speed int) error {
	switch command {
	case "left", "right", "up", "down", "leftUp", "leftDown", "rightUp", "rightDown", "zoomInc", "zoomDec":
	case "stop":
		speed = 0
	default:
		return fmt.Errorf("baichuan: ptz: unknown command %q", command)
	}
	return c.ptzCommand(ctx, command, speed, "")
}

// PtzPatrol starts or stops the camera touring its patrol (cruise) id, the
// presets it was given in the Reolink app. Same message as PtzControl; the
// fake TrackMix toured its cruise on startPatrol with patrolId 0 (10-30).
func (c *Conn) PtzPatrol(ctx context.Context, start bool, id int) error {
	command, speed := "stopPatrol", 0
	if start {
		command, speed = "startPatrol", 64
	}
	return c.ptzCommand(ctx, command, speed, fmt.Sprintf("<patrolId>%d</patrolId>\n", id))
}

func (c *Conn) ptzCommand(ctx context.Context, command string, speed int, extra string) error {
	body := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n<PtzControl version=\"1.1\">\n<channelId>%d</channelId>\n<speed>%d</speed>\n<command>%s</command>\n%s</PtzControl>\n</body>\n",
		c.opts.Channel, speed, command, extra)
	if err := c.SetConfig(MsgIDPtzControl, []byte(body)); err != nil {
		return fmt.Errorf("baichuan: ptz %s: %w", command, err)
	}
	if _, status, err := awaitReply(ctx, c, MsgIDPtzControl); err != nil {
		return fmt.Errorf("baichuan: ptz %s: awaiting ack: %w", command, err)
	} else if status != 0 && status != 200 {
		return &PtzRefused{What: "ptz " + command, Status: int(status)}
	}
	return nil
}

// PtzZoomRange reads the zoom lens's range and position (message 294). A
// camera with no zoom answers otherwise, or with an empty range.
func (c *Conn) PtzZoomRange(ctx context.Context) (lo, hi, cur int, err error) {
	doc, status, err := ReadConfig(ctx, c, MsgIDZoomFocusGet)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("baichuan: zoom: %w", err)
	}
	if status != 200 && status != 0 {
		return 0, 0, 0, fmt.Errorf("baichuan: zoom: camera answered status %d", status)
	}
	var d struct {
		Min int `xml:"PtzZoomFocus>zoom>minPos"`
		Max int `xml:"PtzZoomFocus>zoom>maxPos"`
		Cur int `xml:"PtzZoomFocus>zoom>curPos"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return 0, 0, 0, fmt.Errorf("baichuan: zoom: %w", err)
	}
	return d.Min, d.Max, d.Cur, nil
}

// Preset is one saved camera position.
type Preset struct {
	ID   int    `xml:"id"`
	Name string `xml:"name"`
}

// PtzPresets reads the saved positions (message 190) and how many the camera
// can hold.
func (c *Conn) PtzPresets(ctx context.Context) ([]Preset, int, error) {
	doc, status, err := ReadConfig(ctx, c, MsgIDPtzPreset2)
	if err != nil {
		return nil, 0, fmt.Errorf("baichuan: presets: %w", err)
	}
	if status != 200 && status != 0 {
		return nil, 0, fmt.Errorf("baichuan: presets: camera answered status %d", status)
	}
	var d struct {
		Max  int      `xml:"PtzPreset>maxPresetNum"`
		List []Preset `xml:"PtzPreset>presetList>preset"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return nil, 0, fmt.Errorf("baichuan: presets: %w", err)
	}
	return d.List, d.Max, nil
}

// PtzPreset saves the current position as preset id (command "setPos") or
// moves to it ("toPos"). The document is the client's (message 19).
func (c *Conn) PtzPreset(ctx context.Context, command string, id int, name string) error {
	if command != "setPos" && command != "toPos" {
		return fmt.Errorf("baichuan: preset: unknown command %q", command)
	}
	var esc strings.Builder
	if err := xml.EscapeText(&esc, []byte(name)); err != nil {
		return err
	}
	body := fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n<PtzPreset version=\"1.1\">\n<channelId>%d</channelId>\n<presetList>\n<preset>\n<id>%d</id>\n<command>%s</command>\n<name>%s</name>\n</preset>\n</presetList>\n</PtzPreset>\n</body>\n",
		c.opts.Channel, id, command, esc.String())
	status, err := WriteConfig(ctx, c, MsgIDPtzPreset, []byte(body))
	if err != nil {
		return fmt.Errorf("baichuan: preset %s: %w", command, err)
	}
	if status != 0 && status != 200 {
		return &PtzRefused{What: "preset " + command, Status: int(status)}
	}
	return nil
}

// PtzGuard is the guard position the camera returns to when left alone:
// whether it does (Enable), after how many seconds (Timeout), and whether a
// guard position has been set at all (Valid). Doc is the camera's own
// document, kept so a write echoes it back with only these fields changed.
type PtzGuard struct {
	Enable  bool
	Timeout int
	Valid   bool
	Doc     []byte
}

// GetPtzGuard reads the guard (message 332).
func (c *Conn) GetPtzGuard(ctx context.Context) (PtzGuard, error) {
	doc, status, err := ReadConfig(ctx, c, MsgIDPtzGuard2)
	if err != nil {
		return PtzGuard{}, fmt.Errorf("baichuan: guard: %w", err)
	}
	if status != 200 && status != 0 {
		return PtzGuard{}, fmt.Errorf("baichuan: guard: camera answered status %d", status)
	}
	return parseGuard(doc)
}

func parseGuard(doc []byte) (PtzGuard, error) {
	var d struct {
		Timeout int `xml:"PtzGuard>timeout"`
		Enable  int `xml:"PtzGuard>benable"`
		Valid   int `xml:"PtzGuard>bvalid"`
	}
	if err := xml.Unmarshal(doc, &d); err != nil {
		return PtzGuard{}, fmt.Errorf("baichuan: guard: %w", err)
	}
	return PtzGuard{Enable: d.Enable == 1, Timeout: d.Timeout, Valid: d.Valid == 1, Doc: doc}, nil
}

// SetPtzGuard writes doc (the camera's own guard document, edited) to the
// guard write (message 331).
func (c *Conn) SetPtzGuard(ctx context.Context, doc []byte) error {
	status, err := WriteConfig(ctx, c, MsgIDPtzGuard, doc)
	if err != nil {
		return fmt.Errorf("baichuan: guard: %w", err)
	}
	if status != 0 && status != 200 {
		return fmt.Errorf("baichuan: guard: camera answered status %d", status)
	}
	return nil
}

// Calibrate is the Reolink client's pan/tilt self test: MsgIDPtzAutoTest2
// (341) starts it, MsgIDPtzAutoTest (340) reads PtzAutoTest/testStat. Both
// carry only the channel. Captured from the client on an Argus PT Ultra:
// 341 sent cold answered 400, sent after a 340 read (as the client does)
// 200, and testStat went from 0 to 2 when it finished.

// PtzCalibrateState reads the self test's state (testStat; 2 when one has
// finished).
func (c *Conn) PtzCalibrateState(ctx context.Context) (int, error) {
	if err := c.RawChannelRequest(MsgIDPtzAutoTest); err != nil {
		return 0, err
	}
	doc, status, err := awaitReply(ctx, c, MsgIDPtzAutoTest)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, &PtzRefused{What: "calibrate state", Status: int(status)}
	}
	var v struct {
		TestStat int `xml:"PtzAutoTest>testStat"`
	}
	if err := xml.Unmarshal(doc, &v); err != nil {
		return 0, fmt.Errorf("baichuan: calibrate state: %w", err)
	}
	return v.TestStat, nil
}

// calibrateAnswerWait is how long PtzCalibrate waits for an answer to 341
// before taking silence as a start. A var so a test can shorten it.
var calibrateAnswerWait = 2 * time.Second

// PtzCalibrate starts the self test, reading its state first as the client
// does (the camera refuses a cold start). It reports whether the camera
// answered 341 at all.
//
// A TrackMix answers 341 only when the sweep has finished, a minute or so
// later, and until then answers nothing else on this connection either (fake
// hardware, 10-30: a 340 on the same session went unanswered while one on a
// fresh session read testStat 1). Waiting for the answer under the 15 s idle
// timeout reported every calibrate as refused while the camera calibrated.
// So a refusal within calibrateAnswerWait is still a refusal, and silence
// means it started; the caller confirms with PtzCalibrateState on a new
// connection, and closes this one.
func (c *Conn) PtzCalibrate(ctx context.Context) (answered bool, err error) {
	if _, err := c.PtzCalibrateState(ctx); err != nil {
		return false, err
	}
	if err := c.RawChannelRequest(MsgIDPtzAutoTest2); err != nil {
		return false, err
	}
	wctx, cancel := context.WithTimeout(ctx, calibrateAnswerWait)
	defer cancel()
	_, status, err := awaitReply(wctx, c, MsgIDPtzAutoTest2)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return false, nil
		}
		return false, err
	}
	if status != 200 {
		return true, &PtzRefused{What: "calibrate", Status: int(status)}
	}
	return true, nil
}
