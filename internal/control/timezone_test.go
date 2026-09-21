package control

import "testing"

// TestTheTimezoneLabelCountsTheOtherWay pins the sign convention, which is
// the whole reason this helper exists: the camera counts seconds WEST of
// UTC, and people say the offset EAST. 21600 west is UTC-06:00, not
// UTC+06:00, and getting that backwards would put every camera on the
// wrong side of the world while looking perfectly reasonable.
//
// The anchor case is measured, not assumed: a camera reporting 21600 was
// showing 09:15:29 in its overlay when the host clock read 14:15:34 UTC.
func TestTheTimezoneLabelCountsTheOtherWay(t *testing.T) {
	cases := []struct {
		secondsWest int
		want        string
	}{
		{21600, "UTC−06:00"},  // US Central standard, the measured case
		{18000, "UTC−05:00"},  // US Eastern standard
		{0, "UTC+00:00"},      // UTC itself
		{-3600, "UTC+01:00"},  // one hour EAST is negative west
		{-19800, "UTC+05:30"}, // India, to prove half hours survive
		{12600, "UTC−03:30"},  // Newfoundland, a western half hour
		{-50400, "UTC+14:00"}, // the far end of the range
	}
	for _, tc := range cases {
		if got := utcOffsetLabel(tc.secondsWest); got != tc.want {
			t.Errorf("utcOffsetLabel(%d) = %q, want %q", tc.secondsWest, got, tc.want)
		}
	}
}

// TestTheTimezoneLabelUsesARealMinus checks the sign is U+2212 rather than
// a hyphen. It sits directly beside digits at small sizes, where a hyphen
// is easy to lose entirely, and losing it turns UTC-06:00 into UTC 06:00.
func TestTheTimezoneLabelUsesARealMinus(t *testing.T) {
	got := utcOffsetLabel(21600)
	if got[3] != 0xe2 {
		t.Fatalf("utcOffsetLabel(21600) = %q, want a U+2212 minus before the digits", got)
	}
}

// TestEveryTimezoneChoiceLabelsItsOwnNumber guards the one way a picker
// like this rots: a hand written list whose labels stop matching the
// values they post. Built from the numbers, the two cannot drift.
func TestEveryTimezoneChoiceLabelsItsOwnNumber(t *testing.T) {
	choices := timeZoneChoices()
	if len(choices) < 20 {
		t.Fatalf("got %d offsets, want the world's whole and half hours", len(choices))
	}
	seen := make(map[int]bool, len(choices))
	for _, c := range choices {
		if seen[c.Seconds] {
			t.Errorf("offset %d appears twice", c.Seconds)
		}
		seen[c.Seconds] = true
		if want := utcOffsetLabel(c.Seconds); c.Label != want {
			t.Errorf("offset %d is labelled %q, want %q", c.Seconds, c.Label, want)
		}
	}
	// The measured camera's own offset must be pickable, or the fleet this
	// was built against cannot use the picker at all.
	if !seen[21600] {
		t.Error("UTC-06:00 (21600 west) is not among the choices")
	}
}
