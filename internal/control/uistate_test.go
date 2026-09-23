package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/server"
)

func TestCameraStateIsTheWorstStream(t *testing.T) {
	for _, tc := range []struct {
		states []string
		want   string
	}{
		{nil, "idle"},
		{[]string{"streaming", "streaming"}, "streaming"},
		{[]string{"streaming", "novideo"}, "novideo"},
		{[]string{"novideo", "reconnecting"}, "reconnecting"},
		{[]string{"reconnecting", "down", "streaming"}, "down"},
	} {
		var rows []row
		for _, s := range tc.states {
			rows = append(rows, row{State: s})
		}
		if got := cameraState(rows); got != tc.want {
			t.Errorf("cameraState(%v) = %q, want %q", tc.states, got, tc.want)
		}
	}
}

func card(camera, state string, streams ...streamLine) statusCard {
	return statusCard{Camera: camera, State: state, Streams: streams}
}

func line(state string, st server.StreamStatus) streamLine {
	return streamLine{row: row{State: state, StreamStatus: st}}
}

func TestAlertsCallOutDownAndLongSilentCameras(t *testing.T) {
	got := alerts([]statusCard{
		card("ok", "streaming", line("streaming", server.StreamStatus{})),
		card("blip", "reconnecting", line("reconnecting", server.StreamStatus{Restarts: 1, LastFrameAgeSeconds: 20})),
		card("gone", "reconnecting", line("reconnecting", server.StreamStatus{Restarts: 42, LastError: "no route to host", LastFrameAgeSeconds: 740})),
		card("never", "down", line("down", server.StreamStatus{})),
	})
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2 (gone, never): %+v", len(got), got)
	}
	if got[0].Camera != "gone" || got[0].Sentence != "gone has recorded nothing for 12 minutes." || got[0].Detail != "no route to host · 42 restarts" {
		t.Errorf("reconnecting camera alert = %+v", got[0])
	}
	if got[1].Camera != "never" || got[1].Sentence != "never is not connected." {
		t.Errorf("down camera alert = %+v", got[1])
	}
	if n := healthyCount([]statusCard{card("a", "streaming"), card("b", "novideo"), card("c", "streaming")}); n != 2 {
		t.Errorf("healthyCount = %d, want 2", n)
	}
}

func TestProbeJSONListsOnlyStreamsThatCanBePulled(t *testing.T) {
	w := httptest.NewRecorder()
	writeProbeJSON(w, CameraReport{Streams: []StreamReport{
		{Name: "main", Codec: "hevc", Width: 3840, Height: 2160},
		{Name: "sub", Codec: "h264", Width: 640, Height: 360},
		{Name: "extern", Absent: true},
	}})
	var got probeJSON
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" || len(got.Streams) != 2 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.Streams[0].Detail, "browsers cannot play") || strings.Contains(got.Streams[1].Detail, "browsers cannot play") {
		t.Errorf("only the HEVC stream should carry the browser note: %+v", got.Streams)
	}

	w = httptest.NewRecorder()
	writeProbeJSON(w, CameraReport{Err: "wrong password"})
	got = probeJSON{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Error != "wrong password" || got.Streams == nil || len(got.Streams) != 0 {
		t.Errorf("a failed probe = %+v, want the error and an empty list", got)
	}
}

func TestChoiceKeepsACameraValueItDoesNotList(t *testing.T) {
	f := Field{XPath: "LedState/state", Kind: "choice", Choices: []Choice{{"auto", "Auto"}, {"open", "On"}, {"close", "Off"}}}

	v := cameraPage{Values: map[string]string{"LedState/state": "open"}}.Choice(f)
	if len(v.Options) != 3 || !v.Options[1].Selected || v.NotRead {
		t.Errorf("a listed value: %+v", v)
	}

	v = cameraPage{Values: map[string]string{"LedState/state": "blink"}}.Choice(f)
	if len(v.Options) != 4 || v.Options[3].Value != "blink" || !v.Options[3].Selected {
		t.Errorf("an unlisted value must be offered as its own selected stop: %+v", v)
	}

	v = cameraPage{Values: map[string]string{}}.Choice(f)
	if !v.NotRead || len(v.Options) != 3 {
		t.Errorf("nothing read: %+v", v)
	}
	for _, o := range v.Options {
		if o.Selected {
			t.Errorf("nothing read, yet %q is selected", o.Value)
		}
	}
}

func TestDefaultsComeFromTheCamerasOwnInitialValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "GetImage":
			w.Write([]byte(`[{"cmd":"GetImage","code":0,"value":{"Image":{"bright":140,"contrast":128,"saturation":128}},` +
				`"initial":{"Image":{"bright":128,"contrast":128,"saturation":128,"hue":128}}}]`))
		case "GetOsd":
			w.Write([]byte(`[{"cmd":"GetOsd","code":0,"value":{},` +
				`"initial":{"Osd":{"osdChannel":{"enable":1,"pos":"Lower Right"},"osdTime":{"enable":1,"pos":"Top Center"}}}}]`))
		default:
			w.Write([]byte(`[{"cmd":"x","code":1,"error":{"detail":"not supported","rspCode":-9}}]`))
		}
	}))
	defer srv.Close()
	c, err := cgi.Dial(strings.TrimPrefix(srv.URL, "http://"), "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	got := readDefaults(context.Background(), c)
	want := map[string]string{
		"VideoInput/bright":       "128",
		"VideoInput/contrast":     "128",
		"VideoInput/saturation":   "128",
		"OsdChannelName/enable":   "1",
		"OsdChannelName/topLeftX": osdFarEdge + "," + osdFarEdge,
		"OsdDatetime/enable":      "1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("default for %s = %q, want %q", k, got[k], v)
		}
	}
	// Top centre has no corner in the Baichuan table: no reset, not a guess.
	if v, ok := got["OsdDatetime/topLeftX"]; ok {
		t.Errorf("a centred default was mapped to %q", v)
	}

	bright := Field{XPath: "VideoInput/bright", Kind: "number"}
	p := cameraPage{Defaults: got, Values: map[string]string{"VideoInput/bright": "140"}}
	if p.Reset(bright) != "128" {
		t.Errorf("a changed brightness offers no reset")
	}
	p.Values["VideoInput/bright"] = "128"
	if p.Reset(bright) != "" {
		t.Errorf("a brightness already at its default still offers a reset")
	}
	pos := Field{XPath: "OsdChannelName/topLeftX", YPath: "OsdChannelName/topLeftY", Kind: "position"}
	p.Values["OsdChannelName/topLeftX"], p.Values["OsdChannelName/topLeftY"] = osdFarEdge, osdFarEdge
	if p.Reset(pos) != "" {
		t.Errorf("a position already at its default still offers a reset")
	}
	if l := p.ResetLabel(pos, osdFarEdge+","+osdFarEdge); l != "bottom right" {
		t.Errorf("position reset label = %q", l)
	}
}
