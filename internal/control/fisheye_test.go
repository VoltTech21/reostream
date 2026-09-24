package control

import "testing"

// The four view modes are the camera's own numbering, and the page posts
// names rather than numbers so a form value cannot quietly mean a
// different mode than it reads.
func TestFisheyeOptionsRoundTrip(t *testing.T) {
	for _, o := range fisheyeOptions {
		got, ok := fisheyeOptionByValue(o.Value)
		if !ok {
			t.Errorf("%q is offered but not accepted back", o.Value)
			continue
		}
		if got.Mode != o.Mode {
			t.Errorf("%q came back as mode %d, want %d", o.Value, got.Mode, o.Mode)
		}
		if fisheyeValueByMode(o.Mode) != o.Value {
			t.Errorf("mode %d reads back as %q, want %q", o.Mode, fisheyeValueByMode(o.Mode), o.Value)
		}
	}
	if _, ok := fisheyeOptionByValue("panoramic"); ok {
		t.Error("a value nobody offers was accepted")
	}
}

// A mode the page has no name for must still render as something, rather
// than as an empty control that looks like a camera with no view set.
func TestAnUnknownFisheyeModeStillReads(t *testing.T) {
	if got := fisheyeValueByMode(9); got == "" {
		t.Error("an unknown mode reads as empty")
	}
}

// The confirmation has to name both consequences. Rebooting a camera is
// recoverable by waiting; zones pointing at the wrong part of the picture
// is not recoverable at all without redrawing them, and an operator who
// was not told will not know to.
func TestTheFisheyeConfirmationNamesBothConsequences(t *testing.T) {
	for _, want := range []string{"reboot", "zone"} {
		if !contains(fisheyeConfirmReason, want) {
			t.Errorf("the confirmation does not mention %q: %s", want, fisheyeConfirmReason)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
