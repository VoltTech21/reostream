package control

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
	"golang.org/x/net/html"
)

// Opening a battery camera's Live tab must not talk to the camera: reading
// its name woke it, and the wake watch logged that as motion and sent a
// false motion event (cam-10 Argus, 11-03). What was read last is shown
// instead, and Settings, which someone opened on purpose, still reads it.
func TestOpeningABatteryCamerasPageDoesNotWakeIt(t *testing.T) {
	skipIdent = false
	t.Cleanup(func() { skipIdent = true })
	path := filepath.Join(t.TempDir(), "config.toml")
	cfgText := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n" +
		"[[camera]]\nname = \"porch\"\naddress = \"192.0.2.30\"\nusername = \"admin\"\npassword = \"\"\nuid = \"95270000TESTUID1\"\n"
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Dial: func(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
			dials.Add(1)
			return nil, errors.New("asleep")
		},
	})
	cam := Camera{Name: "porch", Address: "192.0.2.30"}
	identMu.Lock()
	// Read an hour ago, so long past identTTL.
	identCache[cam.Name+"@"+cam.Address] = camIdent{DeviceName: "Front porch", Model: "Argus PT Ultra", at: time.Now().Add(-time.Hour)}
	identMu.Unlock()
	t.Cleanup(func() {
		identMu.Lock()
		delete(identCache, cam.Name+"@"+cam.Address)
		identMu.Unlock()
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	get := func(p string) string {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d\n%s", p, resp.StatusCode, b)
		}
		return string(b)
	}
	body := get("/cameras/porch")
	if n := dials.Load(); n != 0 {
		t.Fatalf("opening the battery camera's Live tab dialed it %d times", n)
	}
	if !strings.Contains(body, "Argus PT Ultra") {
		t.Errorf("the Live tab does not show what was read last:\n%s", body)
	}
	get("/cameras/porch/settings/image")
	if dials.Load() == 0 {
		t.Errorf("opening Settings did not read the camera")
	}
}

// Playing a battery camera wakes it, and its Live tab used to start the
// sub stream on open, so every look at the page cost battery (11-04). A
// sleeping one now shows its last picture with a play button and wakes
// only on a click; one kept streaming (always_on) plays at once as before.
func TestABatteryCamerasLiveTabWaitsForPlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfgText := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n" +
		"[[camera]]\nname = \"porch\"\naddress = \"192.0.2.30\"\nusername = \"admin\"\npassword = \"\"\nuid = \"95270000TESTUID1\"\n\n" +
		"[[camera]]\nname = \"argus\"\naddress = \"192.0.2.31\"\nusername = \"admin\"\npassword = \"\"\nuid = \"95270000TESTUID2\"\nalways_on = true\n"
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	hubs := map[string]*hub.Hub{}
	for _, n := range []string{"porch/sub", "porch/main", "argus/sub", "argus/main"} {
		h := hub.New(4)
		h.SetOnDemand(true)
		hubs[n] = h
	}
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Hubs:            server.StaticHubs(hubs),
		Dial: func(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
			return nil, errors.New("asleep")
		},
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	get := func(p string) string {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d\n%s", p, resp.StatusCode, b)
		}
		return string(b)
	}
	porch := get("/cameras/porch")
	for _, want := range []string{`data-wake="click"`, `class="play-btn live-wake"`, "press play to wake it (uses battery)", "awake while it plays"} {
		if !strings.Contains(porch, want) {
			t.Errorf("sleeping battery camera's Live tab: no %s", want)
		}
	}
	argus := get("/cameras/argus")
	if strings.Contains(argus, `data-wake="click"`) || strings.Contains(argus, "live-wake") {
		t.Errorf("an always_on battery camera's Live tab waits for a click; it streams anyway")
	}

	// Once it has woken, the picture is the kept keyframe, under the same
	// note as its fleet tile.
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/porch", nil), "camera.html",
		cameraPage{Title: "porch", Camera: Camera{Name: "porch"}, Head: camHead{Tab: "live"}, Battery: true,
			WakeOnClick: true, Still: streamPrefix + "/porch_sub.still.ts", StillNote: "Asleep · last picture 10:42",
			Streams: []liveStream{{Name: "sub", Codec: "H264", Playable: true}}})
	body := w.Body.String()
	for _, want := range []string{`data-src="/stream/porch_sub.still.ts"`, "Asleep · last picture 10:42"} {
		if !strings.Contains(body, want) {
			t.Errorf("Live tab with a kept picture: no %s", want)
		}
	}

	js, err := fs.ReadFile(assetFS, "assets/live.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"box.dataset.wake === 'click'", "reostreamSnapshots([stillBox])", "} else if (first) pick(first);"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("live.js does not hold a sleeping battery camera back until play: no %s", want)
		}
	}
}

// The sleeping battery camera's Live tab said "asleep" twice and showed it
// once: the hint "asleep until you press play" sat inside the player, under
// the still and its own "Asleep · last picture" note, so what was seen was
// the note, and the wake costing battery was not seen at all (11-04, at
// 390x844 and desktop). Now the picture says only what it is, and one line
// under the player, never covered, says what play does; live.js changes it
// once the camera is awake. An always_on camera is not "awake only while
// this tab is open".
func TestABatteryCamerasLiveTabSaysItOnce(t *testing.T) {
	render := func(page cameraPage) *html.Node {
		t.Helper()
		w := httptest.NewRecorder()
		s := newTestServer(t, Options{AllowNoPassword: true})
		s.render(w, httptest.NewRequest("GET", "/cameras/porch", nil), "camera.html", page)
		doc, err := html.Parse(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	hasClass := func(n *html.Node, c string) bool {
		for _, a := range n.Attr {
			if a.Key == "class" && slices.Contains(strings.Fields(a.Val), c) {
				return true
			}
		}
		return false
	}
	var find func(n *html.Node, c string) []*html.Node
	find = func(n *html.Node, c string) []*html.Node {
		var out []*html.Node
		if n.Type == html.ElementNode && hasClass(n, c) {
			out = append(out, n)
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			out = append(out, find(k, c)...)
		}
		return out
	}
	var text func(n *html.Node) string
	text = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		var b strings.Builder
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			b.WriteString(text(k))
		}
		return b.String()
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	page := cameraPage{Title: "porch", Camera: Camera{Name: "porch"}, Head: camHead{Tab: "live"}, Battery: true,
		WakeOnClick: true, Streams: []liveStream{{Name: "sub", Codec: "H264", Playable: true}}}
	for _, still := range []bool{true, false} {
		p := page
		wantNote := "Asleep"
		if still {
			p.Still, p.StillNote = streamPrefix+"/porch_sub.still.ts", "Asleep · last picture 10:42"
			wantNote = p.StillNote
		}
		doc := render(p)
		players := find(doc, "live-player")
		if len(players) != 1 {
			t.Fatalf("still=%v: %d players", still, len(players))
		}
		inPlayer := strings.Join(strings.Fields(text(players[0])), " ")
		if strings.Contains(strings.ToLower(inPlayer), "press play") || len(find(players[0], "live-battery")) != 0 {
			t.Errorf("still=%v: the play hint is inside the player, under the still: %q", still, inPlayer)
		}
		if notes := find(players[0], "still-note"); len(notes) != 1 || text(notes[0]) != wantNote {
			t.Errorf("still=%v: the picture's note is not %q: %q", still, wantNote, inPlayer)
		}
		hints := find(doc, "live-battery")
		if len(hints) != 1 {
			t.Fatalf("still=%v: %d battery hints, want 1", still, len(hints))
		}
		if h := text(hints[0]); !strings.Contains(h, "press play to wake it") || !strings.Contains(h, "uses battery") {
			t.Errorf("still=%v: the hint does not say what play does: %q", still, h)
		}
		if a := attr(hints[0], "data-awake"); !strings.Contains(a, "awake while it plays") {
			t.Errorf("still=%v: no awake wording for live.js: %q", still, a)
		}
	}
	p := page
	p.WakeOnClick = false
	if hints := find(render(p), "live-battery"); len(hints) != 0 {
		t.Errorf("an always_on battery camera is told it is awake only while the tab is open: %q", text(hints[0]))
	}
	js, err := fs.ReadFile(assetFS, "assets/live.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "wakeHint.dataset.awake") {
		t.Errorf("live.js does not change the hint once the camera is awake")
	}
}
