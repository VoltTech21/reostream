package control

import "fmt"

// The camera reports its timezone as a whole number of SECONDS WEST of
// UTC: 21600 is six hours west, UTC-06:00. Positive is behind UTC, which
// is the opposite sign from the "UTC-6" people say out loud, and is why
// the raw field on its own reads as a mystery.
//
// Established from hardware, not documentation. A camera reporting 21600
// was showing 09:15:29 in its overlay at the moment a frame was pulled
// from it, and the host clock read 09:15:34 CDT, 14:15:34 UTC. The camera
// was therefore displaying UTC-05:00 while its field said 21600, so the
// field is the STANDARD offset and the camera applies its own daylight
// saving rule on top of it. This page does not write that rule: setTimeZone
// changes the timeZone key and nothing else.
//
// utcOffsetLabel renders that field the way a person reads a timezone.
func utcOffsetLabel(secondsWestOfUTC int) string {
	// Negated, because the field counts west and the label counts east.
	off := -secondsWestOfUTC
	sign := "+"
	if off < 0 {
		sign = "\u2212" // a real minus, not a hyphen: it sits beside digits
		off = -off
	}
	h := off / 3600
	m := (off % 3600) / 60
	return fmt.Sprintf("UTC%s%02d:%02d", sign, h, m)
}

// timeZoneChoice is one entry in the picker beside the raw field. The raw
// field stays editable and authoritative: these are the offsets in common
// use, not the only ones a camera will accept, and a camera already
// holding something else must not have it rounded away by a dropdown.
type timeZoneChoice struct {
	Seconds int
	Label   string
}

// timeZoneChoices is every whole and half hour offset in use, west to
// east. Built rather than written out so the labels cannot drift from the
// numbers they describe.
func timeZoneChoices() []timeZoneChoice {
	halves := []int{
		-12 * 3600, -11 * 3600, -10 * 3600, -9*3600 - 1800, -9 * 3600,
		-8 * 3600, -7 * 3600, -6 * 3600, -5 * 3600, -4 * 3600,
		-3*3600 - 1800, -3 * 3600, -2 * 3600, -1 * 3600, 0,
		1 * 3600, 2 * 3600, 3 * 3600, 3*3600 + 1800, 4 * 3600,
		4*3600 + 1800, 5 * 3600, 5*3600 + 1800, 6 * 3600, 7 * 3600,
		8 * 3600, 9 * 3600, 9*3600 + 1800, 10 * 3600, 11 * 3600,
		12 * 3600, 13 * 3600,
	}
	out := make([]timeZoneChoice, 0, len(halves))
	for _, east := range halves {
		// Stored west-positive, the way the camera holds it.
		west := -east
		out = append(out, timeZoneChoice{Seconds: west, Label: utcOffsetLabel(west)})
	}
	return out
}
