package control_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
)

// TestURLsPageShowsAConfigLoadError is Finding 6 from the 2026-09-10
// review: serveURLs set page.Error on a LoadRaw failure, but the template
// never rendered it, so a first-run user with a broken config got a
// silently empty Frigate block on the one page that exists to hand them
// URLs.
func TestURLsPageShowsAConfigLoadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not valid toml [[[\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: path})
	resp, err := http.Get(ts.URL + "/setup/urls")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Could not read the config") {
		t.Fatalf("urls page does not surface the config load error: %s", body)
	}
}
