package control

import (
	"io/fs"
	"regexp"
	"testing"
)

// live.js is one function, and a var anywhere in it is that function's
// variable. The pan and tilt card's "var stop" (stop moving) replaced the
// player's "function stop" (stop playing) on every camera with a pad, so
// switching streams on a TrackMix never tore the old player down: each
// switch left one more player feeding a removed SourceBuffer, about 20
// uncaught "SourceBuffer ... buffered" errors a second until the page was
// left (11-03). No name may be declared both ways.
func TestLiveJSDeclaresNoFunctionTwice(t *testing.T) {
	b, err := fs.ReadFile(assetFS, "assets/live.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	funcs := map[string]bool{}
	for _, m := range regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`).FindAllStringSubmatch(js, -1) {
		funcs[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\bvar\s+([A-Za-z_$][\w$]*)\s*=`).FindAllStringSubmatch(js, -1) {
		if funcs[m[1]] {
			t.Errorf("live.js declares %q as a function and as a var; the var replaces the function", m[1])
		}
	}
}

// The pan and tilt card sends its commands one at a time through one
// promise chain. Chained with a bare then, one refused command (the
// emulator answered 502, 11-03) left the chain rejected, and every later
// move, stop and preset on the page was silently skipped until a reload.
// Every step added to the chain must first recover from the one before.
func TestLiveJSPTZQueueSurvivesARefusal(t *testing.T) {
	b, err := fs.ReadFile(assetFS, "assets/live.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	steps := regexp.MustCompile(`\bqueue\s*=\s*queue\s*\.\s*(\w+)\s*\(`).FindAllStringSubmatch(js, -1)
	if len(steps) == 0 {
		t.Fatal("live.js no longer chains PTZ commands through queue; update this test")
	}
	for _, m := range steps {
		if m[1] != "catch" {
			t.Errorf("live.js chains a PTZ step with queue = queue.%s(...): one refused command then drops every later one", m[1])
		}
	}
}
