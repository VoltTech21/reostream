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
	// Start from a config that loads, then break it, which is the order
	// this actually happens in: the daemon came up on a good config and
	// somebody edited it. Starting from the broken one would not reach
	// this page at all -- a config that cannot be read is not a claimed
	// install, so the claim gate answers first, on purpose.
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: path})
	if resp, err := http.Get(ts.URL + "/setup"); err == nil {
		resp.Body.Close()
	}
	if err := os.WriteFile(path, []byte("this is not valid toml [[[\n"), 0o600); err != nil {
		t.Fatal(err)
	}

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
