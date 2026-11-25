package control

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// After a save, settings.js puts the camera's fresh row in place of the
// saved one, finding it in the reply by its form action and position. The
// reply's flash carries an undo form posting to that same action, ahead of
// every control, so counting it swapped the saved control for the undo:
// choosing a floodlight mode left a lone "undo" where the four stops had
// been (#56), and in a block of rows sharing one write action each save
// showed the row before it. Every lookup by action must leave it out.
func TestSettingsRefreshSkipsTheUndoForm(t *testing.T) {
	b, err := fs.ReadFile(assetFS, "assets/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	i := strings.Index(js, "function refresh(")
	if i < 0 {
		t.Fatal("settings.js has no refresh; update this test")
	}
	body := js[i:]
	body = body[:strings.Index(body, "\n  }\n")]
	if !strings.Contains(body, ":not(.flash-undo)") {
		t.Errorf("refresh finds forms by action without leaving out the flash's undo form:\n%s", body)
	}
	for _, m := range regexp.MustCompile(`querySelectorAll\(([^)]*)\)`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], "form[action") {
			t.Errorf("refresh builds a form lookup inline (%s); use the one selector that leaves out the undo form", m[1])
		}
	}

	flash, err := fs.ReadFile(templateFS, "templates/flash.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flash), `<form class="flash-undo"`) {
		t.Error("the flash's undo form is no longer class flash-undo, which is how refresh tells it apart")
	}
}

// The zone editor's brush (Ignore / Detect) is a segmented group like the
// settings' stops, and settings.js saved every such group on change. So
// picking Detect to clear an ignored area saved the zone at once, then the
// save's refresh swapped the editor for a copy with no painter and a player
// that never started: the picture vanished (#47). The brush is marked a
// tool and skipped, and the zone form is kept as it is after a save.
func TestZoneEditorIsNotSavedOrSwappedByTheSettingsScript(t *testing.T) {
	js, err := fs.ReadFile(assetFS, "assets/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := fs.ReadFile(templateFS, "templates/camera.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "'.segmented:not([data-tool])'") {
		t.Error("settings.js saves every segmented group on change, the zone brush included")
	}
	if !strings.Contains(string(js), "!form.hasAttribute('data-keep')") {
		t.Error("settings.js swaps every saved form for the reply's copy, the zone editor included")
	}
	if !regexp.MustCompile(`class="segmented zone-tool"[^>]*\bdata-tool\b`).Match(tpl) {
		t.Error("the zone brush is not marked data-tool")
	}
	if !regexp.MustCompile(`action="\{\{base\}\}/cameras/\{\{\$\.Camera\.Name\}\}/zone"[^>]*\bdata-keep\b`).Match(tpl) {
		t.Error("the zone form is not marked data-keep")
	}
}
