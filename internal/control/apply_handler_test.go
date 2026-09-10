package control_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

type recordingReloader struct {
	mu   sync.Mutex
	cams []config.Camera
	n    int
}

func (r *recordingReloader) Reload(cams []config.Camera) (supervisor.ReloadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cams = cams
	r.n++
	return supervisor.ReloadResult{Added: len(cams)}, nil
}

func TestConfigSaveReloadsTheFleet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)

	rel := &recordingReloader{}
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Supervisor:      rel,
	})

	body := url.Values{"toml": {`listen = "0.0.0.0:8560"

[[camera]]
name = "one"
address = "192.0.2.50"
streams = ["main"]
`}}
	resp, err := http.PostForm(ts.URL+"/config", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.n != 1 {
		t.Fatalf("Reload called %d times, want 1", rel.n)
	}
	if len(rel.cams) != 1 || rel.cams[0].Name != "one" {
		t.Fatalf("Reload got %+v", rel.cams)
	}
}

func TestConfigSaveDoesNotReloadWhenValidationFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600)

	rel := &recordingReloader{}
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Supervisor:      rel,
	})

	resp, err := http.PostForm(ts.URL+"/config", url.Values{"toml": {"nonsense = 1\n"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.n != 0 {
		t.Fatal("a rejected config was applied to the fleet")
	}
}
