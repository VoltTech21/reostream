package control

import (
	"io/fs"
	"sort"
	"testing"
)

// TestOnlyTheListedAssetsAreEmbedded guards the directory that is served
// WITHOUT authentication.
//
// assets/ used to be embedded with a bare `//go:embed assets`, which made
// publishing a file the default for anything that ever landed in that
// directory. That is not theoretical: a creds.toml dropped there was served
// to an unauthenticated request with the whole suite still green, because
// nothing anywhere asserted what the directory contains.
//
// Listing the files in the embed directive fixes the default, and this test
// is what stops the wildcard coming back: adding a file to the directory
// alone changes nothing, and adding one to the directive fails here until
// somebody says out loud that it is meant to be world readable.
func TestOnlyTheListedAssetsAreEmbedded(t *testing.T) {
	want := []string{
		"assets/LICENSE-mpegts.txt",
		"assets/dashboard.js",
		"assets/README.md",
		"assets/mpegts.js",
		"assets/mpegts.js.LICENSE.txt",
		"assets/settings.js",
		"assets/style.css",
	}

	var got []string
	err := fs.WalkDir(assetFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got = append(got, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets: %v", err)
	}
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("embedded assets are %v, want %v -- a file served with no\n"+
			"authentication was added or removed; if that is deliberate, say so here too", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("embedded assets are %v, want %v", got, want)
		}
	}
}
