package baichuan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

const lightNonce = "light-test-nonce-00000000000000"

func floodlightReport(ch0, ch1 int) string {
	return xmlHeader + `<body><FloodlightStatusList version="1.1">` +
		`<FloodlightStatus><channel>0</channel><status>` + itoa(ch0) + `</status><brightness>85</brightness></FloodlightStatus>` +
		`<FloodlightStatus><channel>1</channel><status>` + itoa(ch1) + `</status><brightness>85</brightness></FloodlightStatus>` +
		`</FloodlightStatusList></body>`
}

// sentBody waits for the client's message id to reach cam and returns its
// document section, decrypted.
func sentBody(t *testing.T, cam *fakecam.Camera, id uint32) string {
	t.Helper()
	key := AESKey(lightNonce, "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		var found string
		walkMessages(cam.Received(), func(h Header, body []byte) bool {
			if h.MsgID == id && h.MsgLen > h.PayloadOff {
				if dec, err := AESDecrypt(key, body[h.PayloadOff:]); err == nil {
					found = string(dec)
				}
			}
			return true
		})
		if found != "" {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %d never reached the camera", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The document is the Reolink app's, as captured to a TrackMix, and its
// length is the 177 bytes reocapture saw the app send an original Lumus
// (GitHub #29): the same fields, the same digits.
func TestFloodlightManualIsTheAppsDocument(t *testing.T) {
	on := string(FloodlightManualXML(0, true))
	for _, want := range []string{
		`<FloodlightManual version="1.1">`, "<channelId>0</channelId>", "<status>1</status>", "<duration>180</duration>",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("288 document lacks %s:\n%s", want, on)
		}
	}
	if len(on) != 177 {
		t.Errorf("288 document is %d bytes; the app's is 177", len(on))
	}
	if off := string(FloodlightManualXML(0, false)); !strings.Contains(off, "<status>0</status>") || !strings.Contains(off, "<duration>180</duration>") {
		t.Errorf("off document:\n%s", off)
	}
}

func TestSirenIsTheAppsOneShot(t *testing.T) {
	doc := string(SirenXML(0))
	for _, want := range []string{
		`<audioPlayInfo version="1.1">`, "<channelId>0</channelId>", "<playMode>0</playMode>",
		"<playDuration>10</playDuration>", "<playTimes>1</playTimes>", "<onOff>1</onOff>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("263 document lacks %s:\n%s", want, doc)
		}
	}
}

// The report pushed at login is the state before the command; the answer is
// the report after it. Read the wrong one and "on" is confirmed by a camera
// that is still dark.
func TestSetFloodlightAnswersFromTheReportAfterTheCommand(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(0, 0)},
		statusReply{id: MsgIDFloodlightSet, status: 200, body: ""},
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(1, 0)},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, reported, err := conn.SetFloodlight(context.Background(), true, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reported || !st.On() || st.Brightness != 85 {
		t.Fatalf("got %+v reported=%v, want lit at 85", st, reported)
	}
	if got := sentBody(t, cam, MsgIDFloodlightSet); got != string(FloodlightManualXML(0, true)) {
		t.Fatalf("camera received:\n%s", got)
	}
}

// A report can arrive that does not yet say what was asked (a TrackMix
// sends two, a millisecond apart); the answer is the one that does, and a
// report that never agrees is kept as the last word.
func TestSetFloodlightWaitsForTheReportThatAgrees(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDFloodlightSet, status: 200, body: ""},
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(0, 0)},
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(1, 1)},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, reported, err := conn.SetFloodlight(context.Background(), true, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reported || !st.On() {
		t.Fatalf("got %+v reported=%v, want the second report, lit", st, reported)
	}
}

func TestSetFloodlightKeepsALastReportThatDisagrees(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDFloodlightSet, status: 200, body: ""},
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(1, 1)},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, reported, err := conn.SetFloodlight(context.Background(), false, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !reported || !st.On() {
		t.Fatalf("got %+v reported=%v, want the disagreeing report kept", st, reported)
	}
}

func TestSetFloodlightRefusal(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDFloodlightSet, status: 400, body: ""},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _, err = conn.SetFloodlight(context.Background(), true, time.Second)
	var refused *CommandRefused
	if !errors.As(err, &refused) || refused.Status != 400 {
		t.Fatalf("err = %v, want a 400 refusal", err)
	}
}

func TestFloodlightNowReadsTheLoginReport(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDFloodlightReport, status: 200, body: floodlightReport(0, 1)},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: "", Channel: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, ok, err := conn.FloodlightNow(context.Background(), time.Second)
	if err != nil || !ok || !st.On() || st.Channel != 1 {
		t.Fatalf("got %+v ok=%v err=%v, want channel 1 lit", st, ok, err)
	}
}

func TestFloodlightNowWithNoReport(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok, err := conn.FloodlightNow(context.Background(), 200*time.Millisecond); ok || err != nil {
		t.Fatalf("ok=%v err=%v, want no report and no error", ok, err)
	}
}

func TestSoundSirenSendsTheOneShot(t *testing.T) {
	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, lightNonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDPlayStopAudio, status: 200, body: ""},
	))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SoundSiren(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentBody(t, cam, MsgIDPlayStopAudio); got != string(SirenXML(0)) {
		t.Fatalf("camera received:\n%s", got)
	}
}
