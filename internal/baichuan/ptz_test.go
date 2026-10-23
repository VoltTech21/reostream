package baichuan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// The documents below are what a Reolink E1 (firmware v3.2.0.4858) accepted
// and acted on: the pan counter moved with the command and returned with the
// opposite one. The spacing is the camera's own, see marshalDoc.
func TestPTZControlDocument(t *testing.T) {
	got, err := ptzControlXML(0, PTZLeft, 32)
	if err != nil {
		t.Fatal(err)
	}
	want := "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n" +
		"<body>\n" +
		"<PtzControl version=\"1.1\">\n" +
		"<channelId>0</channelId>\n" +
		"<speed>32</speed>\n" +
		"<command>left</command>\n" +
		"</PtzControl>\n" +
		"</body>\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPTZPresetDocument(t *testing.T) {
	got, err := ptzGotoPresetXML(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<PtzPreset version=\"1.1\">",
		"<channelId>0</channelId>",
		"<id>3</id>",
		"<command>toPos</command>",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// Replies as the E1 sends them.
const ptzCurPosReply = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<ptzCurPos version="1.1">
<pPos>1938</pPos>
<tPos>222</tPos>
<zPos>-1</zPos>
</ptzCurPos>
</body>
`

const ptzPresetReply = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<PtzPreset version="1.1">
<channelId>0</channelId>
<maxPresetNum>64</maxPresetNum>
<maxPresetPicNum>64</maxPresetPicNum>
<presetList>
<preset>
<id>0</id>
<name>0</name>
<imageName>preset_00</imageName>
</preset>
<preset>
<id>5</id>
<name>Door</name>
<imageName>preset_05</imageName>
</preset>
</presetList>
</PtzPreset>
</body>
`

func TestParsePTZPosition(t *testing.T) {
	p, err := parsePTZPosition([]byte(ptzCurPosReply))
	if err != nil {
		t.Fatal(err)
	}
	if p != (PTZPosition{Pan: 1938, Tilt: 222, Zoom: -1}) {
		t.Errorf("got %+v", p)
	}
	if _, err := parsePTZPosition([]byte("<body/>")); err == nil {
		t.Error("a reply with no position parsed")
	}
}

func TestParsePTZPresets(t *testing.T) {
	ps, err := parsePTZPresets([]byte(ptzPresetReply))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0] != (PTZPreset{0, "0"}) || ps[1] != (PTZPreset{5, "Door"}) {
		t.Errorf("got %+v", ps)
	}
	// No presets is a valid answer, not an error.
	ps, err = parsePTZPresets([]byte(`<body><PtzPreset version="1.1"><channelId>0</channelId><presetList/></PtzPreset></body>`))
	if err != nil || len(ps) != 0 {
		t.Errorf("empty list: %v %v", ps, err)
	}
}

// A control message is a write: the channel in an extension and the document
// in a second section. Sent as one section a camera answers 421 and does
// nothing, the same trap setconfig_test.go pins down for configuration.
func TestPTZControlIsATwoSectionWrite(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))
	conn, err := Dial(context.Background(), cam.Addr(), Options{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// The fake camera never answers, so the wait ends by context; only the
	// bytes sent matter here.
	_, _ = PTZControl(ctx, conn, PTZStop, 1)

	var found bool
	walkMessages(cam.Received(), func(h Header, _ []byte) bool {
		if h.MsgID != MsgIDPtzControl {
			return true
		}
		found = true
		if h.PayloadOff == 0 || h.MsgLen <= h.PayloadOff {
			t.Errorf("MsgLen %d, PayloadOff %d: not a two section write", h.MsgLen, h.PayloadOff)
		}
		return false
	})
	if !found {
		t.Fatalf("no message %d reached the camera", MsgIDPtzControl)
	}
}
