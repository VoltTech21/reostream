package control

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every live player turns mpegts.js's IO stash off; left on it keeps the
// picture behind for the whole session (#59). The recording player is not
// live and keeps it.
func TestEveryLivePlayerTurnsTheStashOff(t *testing.T) {
	files, err := filepath.Glob("assets/*.js")
	if err != nil {
		t.Fatal(err)
	}
	live := regexp.MustCompile(`mpegts\.createPlayer\(\{[^}]*isLive: true[^}]*\}(.*)`)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "mpegts.js") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range live.FindAllStringSubmatch(string(b), -1) {
			n++
			rest := m[1]
			if !strings.Contains(rest, "reostreamLive") && !strings.Contains(rest, "enableStashBuffer: false") {
				t.Errorf("%s: live player without the stash off: %s", f, strings.TrimSpace(m[0]))
			}
		}
	}
	if n < 4 {
		t.Fatalf("found %d live players, want at least 4: the pattern no longer matches the assets", n)
	}
}
