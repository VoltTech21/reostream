package control

import (
	"io/fs"
	"regexp"
	"testing"
)

// A live player must consume the newest transport data immediately. The
// mpegts.js stash deliberately absorbs network jitter, but on an always-on
// camera it also leaves the picture several hundred milliseconds behind the
// newest decoded frame for the entire session.
func TestPlayerDisablesMPEGTSStashBuffer(t *testing.T) {
	b, err := fs.ReadFile(assetFS, "assets/snapshot.js")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`(?s)mpegts\.createPlayer\(\s*\{[^}]*isLive:\s*true[^}]*\},\s*\{[^}]*enableStashBuffer:\s*false[^}]*\}\s*\)`)
	if !call.Match(b) {
		t.Fatal("snapshot.js does not pass enableStashBuffer: false to the live mpegts.js player")
	}
}
