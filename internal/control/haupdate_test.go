package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

// No test in this package reads GitHub: a relay started with discovery on
// would otherwise call the real releases API. A test that wants a release
// points releasesLatest at its own server.
func init() { releasesLatest = "http://127.0.0.1:1/releases/latest" }

func TestUpdateEntityIsAnnouncedWithTheCameras(t *testing.T) {
	msgs := haDiscoveryMessages("reostream", "v0.2.11", []config.Camera{{Name: "porch", Address: "192.0.2.10", Streams: []string{"main"}}})
	var doc map[string]any
	for _, m := range msgs {
		if m.Topic == "homeassistant/update/reostream-reostream/update/config" {
			if err := json.Unmarshal(m.Payload, &doc); err != nil {
				t.Fatal(err)
			}
		}
	}
	if doc == nil {
		t.Fatalf("no update entity among %d discovery messages", len(msgs))
	}
	for k, v := range map[string]any{
		"unique_id":          "reostream-reostream_update",
		"state_topic":        "reostream/reostream/update",
		"availability_topic": "reostream/status",
		"device_class":       "firmware",
	} {
		if doc[k] != v {
			t.Errorf("%s = %v, want %v", k, doc[k], v)
		}
	}
	dev, _ := doc["device"].(map[string]any)
	if dev["name"] != "reostream" || dev["sw_version"] != "v0.2.11" {
		t.Errorf("device %v, want reostream at v0.2.11", dev)
	}
	if _, ok := doc["command_topic"]; ok {
		t.Error("the entity offers an install command; reostream cannot update itself")
	}
}

func TestUpdateStateWithAndWithoutARelease(t *testing.T) {
	st := map[string]any{}
	json.Unmarshal(haUpdateState("v0.2.10", nil), &st)
	if st["installed_version"] != "v0.2.10" || st["latest_version"] != "v0.2.10" {
		t.Fatalf("with no release read: %v, want latest = installed so no update shows", st)
	}
	st = map[string]any{}
	json.Unmarshal(haUpdateState("v0.2.10", &release{Tag: "v0.2.11", URL: "https://github.com/VoltTech21/reostream/releases/tag/v0.2.11", Body: "Fixes."}), &st)
	if st["latest_version"] != "v0.2.11" || st["release_url"] != "https://github.com/VoltTech21/reostream/releases/tag/v0.2.11" || st["release_summary"] != "Fixes." {
		t.Fatalf("with a release: %v", st)
	}
	st = map[string]any{}
	json.Unmarshal(haUpdateState("v0.2.10", &release{Tag: "v0.2.11", URL: "javascript:alert(1)"}), &st)
	if _, ok := st["release_url"]; ok {
		t.Fatal("a release_url that is not https went through")
	}
}

// Home Assistant refuses a state whose summary is over 255 characters, so
// the notes are cut at a line end and marked as cut.
func TestReleaseSummaryFitsHomeAssistant(t *testing.T) {
	long := strings.Repeat("A line of release notes that goes on.\n", 20)
	s := releaseSummary(long)
	if n := len(s); n > haSummaryMax {
		t.Fatalf("summary is %d bytes, over %d", n, haSummaryMax)
	}
	if !strings.HasSuffix(s, "…") || strings.Contains(s[:len(s)-len("…")], "goes on.\n…") {
		t.Fatalf("summary not cut cleanly: %q", s)
	}
	if releaseSummary("Short.") != "Short." {
		t.Fatal("a short summary was changed")
	}
}

func TestFetchLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "reostream/") {
			t.Errorf("User-Agent %q; GitHub refuses requests without one", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"tag_name":"v0.2.11","name":"v0.2.11","html_url":"https://github.com/VoltTech21/reostream/releases/tag/v0.2.11","body":"Notes"}`))
	}))
	defer srv.Close()
	was := releasesLatest
	releasesLatest = srv.URL
	defer func() { releasesLatest = was }()
	r, err := fetchLatestRelease(context.Background(), srv.Client(), "v0.2.10")
	if err != nil || r.Tag != "v0.2.11" || r.Body != "Notes" {
		t.Fatalf("got %+v, %v", r, err)
	}
	releasesLatest = srv.URL + "/missing"
	srv.Config.Handler = http.NotFoundHandler()
	if _, err := fetchLatestRelease(context.Background(), srv.Client(), "v0.2.10"); err == nil {
		t.Fatal("a 404 read as a release")
	}
}

// update_check = false survives a save of the Events page: the page writes
// the whole [events] section from its form, so a setting with no box there
// would be wiped by the next save.
func TestEventsPageKeepsUpdateCheckOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)
	s := newTestServer(t, noCamera(Options{ConfigPath: path, AllowNoPassword: true}))
	t.Cleanup(func() {
		os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)
		s.syncRelay()
	})
	for _, tc := range []struct {
		form string
		want bool
	}{{"mqtt=127.0.0.1:1&ha_discovery=1", false}, {"mqtt=127.0.0.1:1&ha_discovery=1&update_check=1", true}} {
		req := httptest.NewRequest(http.MethodPost, "/settings/events", strings.NewReader(tc.form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.saveEventsPage(httptest.NewRecorder(), req)
		cfg, err := config.LoadRaw(path)
		if err != nil || cfg.Events == nil {
			t.Fatalf("%s: config after save: %v", tc.form, err)
		}
		if got := cfg.Events.ChecksForUpdates(); got != tc.want {
			t.Errorf("%s: update check %v after save, want %v", tc.form, got, tc.want)
		}
	}
}
