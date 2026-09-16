package control_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// orderingReloader records every camera list it is asked to apply, in the
// order Reload actually ran, and can be told to stall one specific call so a
// test can force two saves to overlap. Without configMu serialising
// writeAndApply, a save whose apply is slow can still have already written
// its file before a second, faster save writes and applies its own list, and
// then the slow save's apply lands last: the file on disk ends up describing
// the fast save while the fleet ends up running the slow one.
type orderingReloader struct {
	mu      sync.Mutex
	applied [][]config.Camera

	// stallName, if set, makes Reload block on stallRelease before applying
	// a camera list containing a camera by that name.
	stallName    string
	stallEntered chan struct{}
	stallRelease chan struct{}
}

func (r *orderingReloader) Reload(cams []config.Camera) (supervisor.ReloadResult, error) {
	if r.stallName != "" {
		for _, c := range cams {
			if c.Name == r.stallName {
				close(r.stallEntered)
				<-r.stallRelease
				break
			}
		}
	}
	r.mu.Lock()
	r.applied = append(r.applied, cams)
	r.mu.Unlock()
	return supervisor.ReloadResult{Added: len(cams)}, nil
}

func (r *orderingReloader) Validate(cams []config.Camera) error {
	return nil
}

func (r *orderingReloader) lastApplied() []config.Camera {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied[len(r.applied)-1]
}

// TestConcurrentConfigSavesDoNotLeaveTheFileAndTheFleetDisagreeing is Finding
// 2 from the 2026-09-10 review. Two POST /config requests save different
// camera lists at once, one deliberately slowed down inside Reload to widen
// the window a missing lock would leave open. The assertion is that whatever
// camera list ends up on disk is the same one that was applied last: the
// file and the running fleet must never describe two different configs.
func TestConcurrentConfigSavesDoNotLeaveTheFileAndTheFleetDisagreeing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// The [control] section is what makes this install claimed; without
	// one, every route below would be answered by the claim gate instead
	// of by the handler under test. See claim.go.
	os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n"), 0o600)

	rel := &orderingReloader{
		stallName:    "slow",
		stallEntered: make(chan struct{}),
		stallRelease: make(chan struct{}),
	}
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		ConfigPath:      path,
		Supervisor:      rel,
	})

	slowTOML := `listen = "0.0.0.0:8560"

[control]
listen = "0.0.0.0:8562"
allow_no_password = true

[[camera]]
name = "slow"
address = "192.0.2.50"
streams = ["main"]
`
	fastTOML := `listen = "0.0.0.0:8560"

[control]
listen = "0.0.0.0:8562"
allow_no_password = true

[[camera]]
name = "fast"
address = "192.0.2.60"
streams = ["main"]
`

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.PostForm(ts.URL+"/config", url.Values{"toml": {slowTOML}})
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}()

	// Wait for the slow save to be inside Reload, mid-apply, before firing
	// the second save. Without configMu, this is exactly the state that lets
	// the second save's write and apply both complete before the first
	// save's apply finally runs.
	select {
	case <-rel.stallEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow save never entered Reload")
	}

	// This must also run in the background: with configMu held for the
	// whole slow save, this request blocks waiting for that lock, and if it
	// were issued synchronously here it would deadlock against the
	// stallRelease close below, which this goroutine is what unblocks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.PostForm(ts.URL+"/config", url.Values{"toml": {fastTOML}})
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}()

	// Give the fast save a chance to reach (and, absent the fix, pass
	// through) writeAndApply before the slow save's Reload call is allowed
	// to finish.
	time.Sleep(100 * time.Millisecond)
	close(rel.stallRelease)
	wg.Wait()

	fileText, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fileHasSlow := strings.Contains(string(fileText), `name = "slow"`)
	fileHasFast := strings.Contains(string(fileText), `name = "fast"`)

	last := rel.lastApplied()
	if len(last) != 1 {
		t.Fatalf("last applied camera list is %+v, want exactly one camera", last)
	}
	lastIsSlow := last[0].Name == "slow"
	lastIsFast := last[0].Name == "fast"

	if fileHasSlow && !lastIsSlow || fileHasFast && !lastIsFast {
		t.Fatalf("file on disk (slow=%v fast=%v) does not match the fleet's last applied config (%q)",
			fileHasSlow, fileHasFast, last[0].Name)
	}
}
