package control

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoTestPostsToARouteThatDoesNotExist is a guard against a whole class
// of test that cannot fail.
//
// Two were found in this package by hand: one posted to /cameras when the
// handler had moved to /cameras/add, so every request answered 405, and the
// test still passed because all it asserted was that nothing was destroyed.
// A test aimed at a route that is not registered exercises the mux's 404
// path and nothing else, and it will keep passing no matter what happens to
// the handler it was written for.
//
// This reads the package's own test sources, pulls out every URL path they
// request, and checks each against the routes Handler actually registers.
// It is deliberately mechanical: the point is that nobody has to notice.
//
// Two limits, both found by trying to make it fail rather than by reasoning
// about it, and both worth knowing before trusting a green run here:
//
// It only sees LITERAL paths. A test that builds its target from a variable
// or a table -- routes_test.go does exactly that -- is invisible to this,
// because the path never appears as a string in the source.
//
// And a path with a wildcard segment in the right position is servable even
// when it looks wrong: /cameras/addd is matched by GET /cameras/{name} and
// is correctly NOT reported. The mistake this catches is a path whose shape
// no pattern has, like an extra or misspelled trailing segment.
func TestNoTestPostsToARouteThatDoesNotExist(t *testing.T) {
	// ts.URL + "/some/path" is how every test in this package builds a
	// request target.
	ref := regexp.MustCompile(`ts\.URL\s*\+\s*"([^"]+)"`)

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		// Skip this file: the doc comment above quotes the very pattern
		// being searched for, as an example of what it looks for.
		if f == "testroutes_test.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(src), -1) {
			path := m[1]
			// Drop a query string; the mux routes on path alone.
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			if path == "" || !strings.HasPrefix(path, "/") {
				continue
			}
			if !someRouteCouldServe(path) {
				t.Errorf("%s requests %q, which no registered route can serve.\n"+
					"A test aimed at an unregistered path exercises the mux's 404 and "+
					"nothing else, and will pass whatever the handler does.", f, path)
			}
		}
	}
}

// someRouteCouldServe reports whether any registered pattern could match
// path. Patterns carry wildcards like {name} and {id}, so this compares
// segment by segment and treats a wildcard as matching anything.
func someRouteCouldServe(path string) bool {
	want := strings.Split(strings.Trim(path, "/"), "/")
	for _, pattern := range routePatterns {
		_, p, ok := strings.Cut(pattern, " ")
		if !ok {
			continue
		}
		// A trailing-slash pattern is a subtree: /assets/ serves
		// everything under it.
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
		p = strings.ReplaceAll(p, "{$}", "")
		got := strings.Split(strings.Trim(p, "/"), "/")
		if len(got) != len(want) {
			continue
		}
		match := true
		for i := range got {
			if strings.HasPrefix(got[i], "{") {
				continue // a wildcard segment
			}
			if got[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
