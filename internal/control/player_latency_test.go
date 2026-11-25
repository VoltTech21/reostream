package control

import (
	"io/fs"
	"regexp"
	"testing"
)

var (
	createPlayer   = regexp.MustCompile(`mpegts\.createPlayer\s*\(`)
	lowLatencyLive = regexp.MustCompile(`(?s)mpegts\.createPlayer\(\s*\{[^}]*isLive:\s*true[^}]*\},\s*\{[^}]*enableStashBuffer:\s*false[^}]*\}\s*\)`)
)

// Every live view goes through reostreamPlayer in snapshot.js. Keeping the raw
// mpegts.js constructor in one place makes the low-latency policy impossible
// to omit from a new dashboard, alignment, or dewarp player by accident.
func TestEveryLiveViewUsesLowLatencyPlayer(t *testing.T) {
	assets, err := fs.ReadDir(assetFS, "assets")
	if err != nil {
		t.Fatal(err)
	}

	for _, asset := range assets {
		name := asset.Name()
		if asset.IsDir() || name == "mpegts.js" || name == "snapshot.js" || name == "rec.js" {
			continue
		}
		b, err := fs.ReadFile(assetFS, "assets/"+name)
		if err != nil {
			t.Fatal(err)
		}
		// Scan the raw source rather than trying to parse JavaScript comments.
		// This is deliberately fail-closed: even writing a direct constructor
		// call in a comment requires updating this policy test.
		if createPlayer.Match(b) {
			t.Errorf("assets/%s creates an mpegts.js player directly instead of using reostreamPlayer", name)
		}
	}

	b, err := fs.ReadFile(assetFS, "assets/snapshot.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(createPlayer.FindAll(b, -1)) != 1 {
		t.Fatal("snapshot.js must be the single live mpegts.js player factory")
	}
	if !lowLatencyLive.Match(b) {
		t.Fatal("reostreamPlayer does not pass enableStashBuffer: false to the live mpegts.js player")
	}
}
