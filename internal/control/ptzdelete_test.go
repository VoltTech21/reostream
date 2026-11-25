package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// fakePresetCamera answers the web API's preset commands. keep makes it
// answer OK to SetPtzPreset and change nothing.
type fakePresetCamera struct {
	srv     *httptest.Server
	mu      sync.Mutex
	enabled map[int]bool
	keep    bool
	sets    []map[string]any
}

func newFakePresetCamera(t *testing.T, saved ...int) *fakePresetCamera {
	f := &fakePresetCamera{enabled: map[int]bool{}}
	for _, id := range saved {
		f.enabled[id] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "Logout":
			w.Write([]byte(`[{"cmd":"Logout","code":0,"value":{"rspCode":200}}]`))
		case "GetPtzPreset":
			list := "["
			for id := 0; id < 8; id++ {
				if id > 0 {
					list += ","
				}
				e := 0
				if f.enabled[id] {
					e = 1
				}
				list += `{"channel":0,"enable":` + strconv.Itoa(e) + `,"id":` + strconv.Itoa(id) + `,"name":"pos` + strconv.Itoa(id+1) + `"}`
			}
			w.Write([]byte(`[{"cmd":"GetPtzPreset","code":0,"value":{"PtzPreset":` + list + `]}}]`))
		case "SetPtzPreset":
			body, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					PtzPreset map[string]any `json:"PtzPreset"`
				} `json:"param"`
			}
			if err := json.Unmarshal(body, &req); err != nil || len(req) != 1 {
				t.Errorf("bad SetPtzPreset body %s: %v", body, err)
				w.Write([]byte(`[{"cmd":"SetPtzPreset","code":1}]`))
				return
			}
			p := req[0].Param.PtzPreset
			f.sets = append(f.sets, p)
			if !f.keep && p["enable"] == float64(0) {
				f.enabled[int(p["id"].(float64))] = false
			}
			w.Write([]byte(`[{"cmd":"SetPtzPreset","code":0,"value":{"rspCode":200}}]`))
		default:
			t.Errorf("fakePresetCamera: unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePresetCamera) server(t *testing.T) *httptest.Server {
	addr := f.srv.URL[len("http://"):]
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: func(Camera) (*cgi.Client, error) { return cgi.Dial(addr, "admin", "") }})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// Deleting a saved view (#54) empties its slot over the web API, with the
// id alone, as the emulated TrackMix took it (11-24), and is confirmed by
// reading the slot back.
func TestDeletePresetEmptiesTheSlot(t *testing.T) {
	cam := newFakePresetCamera(t, 2, 3)
	ts := cam.server(t)
	resp := postForm(t, http.DefaultClient, ts.URL+"/cameras/cam1/ptz/preset", url.Values{"op": {"delete"}, "id": {"3"}})
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 204: %s", resp.StatusCode, b)
	}
	cam.mu.Lock()
	defer cam.mu.Unlock()
	if cam.enabled[3] || !cam.enabled[2] {
		t.Fatalf("camera holds %v; want 3 gone and 2 kept", cam.enabled)
	}
	if len(cam.sets) != 1 || cam.sets[0]["enable"] != float64(0) || cam.sets[0]["id"] != float64(3) || cam.sets[0]["channel"] != float64(0) {
		t.Fatalf("SetPtzPreset sent %v, want one {channel 0, enable 0, id 3}", cam.sets)
	}
}

// A camera that answers OK and keeps the view is not reported as done.
func TestDeletePresetThatStaysIsAnError(t *testing.T) {
	cam := newFakePresetCamera(t, 3)
	cam.keep = true
	ts := cam.server(t)
	resp := postForm(t, http.DefaultClient, ts.URL+"/cameras/cam1/ptz/preset", url.Values{"op": {"delete"}, "id": {"3"}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 when the view is still saved", resp.StatusCode)
	}
}

func TestDeletePresetRefusesABadID(t *testing.T) {
	ts := newFakePresetCamera(t).server(t)
	for _, id := range []string{"", "x", "-1", "256"} {
		resp := postForm(t, http.DefaultClient, ts.URL+"/cameras/cam1/ptz/preset", url.Values{"op": {"delete"}, "id": {id}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q: got %d, want 400", id, resp.StatusCode)
		}
	}
}

// A camera with its web API off gets a reason it can act on, not a bare
// dial error.
func TestDeletePresetWithoutTheWebAPISaysWhatToDo(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"),
		CGIDial: func(Camera) (*cgi.Client, error) { return nil, errors.New("connection refused") }})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	resp := postForm(t, http.DefaultClient, ts.URL+"/cameras/cam1/ptz/preset", url.Values{"op": {"delete"}, "id": {"3"}})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "Turn HTTP on") || !strings.Contains(string(b), "Reolink app") {
		t.Fatalf("got %d %q; want 502 saying to turn HTTP on or use the Reolink app", resp.StatusCode, b)
	}
}
