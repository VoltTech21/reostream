package cgi

import (
	"context"
	"encoding/json"
	"fmt"
)

// Stitch is the dual lens alignment on a two lens camera.
//
// The two fields are not two flavours of the same control. Distance is a
// physical measurement and the moves are offsets, and they correct different
// errors:
//
// The lenses sit a few centimetres apart, so in the region where their views
// overlap an object appears in a different place in each image by an amount
// that depends on how far away it is. Near objects shift a lot, distant ones
// barely. No single alignment is therefore correct for a whole scene: a seam
// can only be made to disappear at one depth, and Distance chooses which.
// Objects much nearer than it duplicate across the seam, objects much further
// are clipped out of it.
//
// XMove and YMove are a fixed nudge of one image against the other, correcting
// mechanical tolerance in how the lens modules are mounted. That error is the
// same whatever the camera is looking at, which is why it is a constant rather
// than a function of depth.
//
// So: a seam misaligned the same way everywhere is XMove and YMove. A seam
// that lines up at one depth and splits at another is Distance, and no value
// fixes every depth at once.
type Stitch struct {
	Distance float64 `json:"distance"`
	XMove    int     `json:"stitchXMove"`
	YMove    int     `json:"stitchYMove"`
}

// StitchLimits is the valid range for each field, as the camera reports it.
type StitchLimits struct {
	Distance struct{ Min, Max float64 }
	XMove    struct{ Min, Max int }
	YMove    struct{ Min, Max int }
}

// GetStitch reads the stitch settings along with the factory defaults and the
// valid range, which is everything needed to adjust it without guessing.
func (c *Client) GetStitch(ctx context.Context, channel int) (current, factory Stitch, limits StitchLimits, err error) {
	value, initial, rng, err := c.Get(ctx, "GetStitch", channel)
	if err != nil {
		return Stitch{}, Stitch{}, StitchLimits{}, err
	}

	var cur, ini struct {
		Stitch Stitch `json:"stitch"`
	}
	if err := json.Unmarshal(value, &cur); err != nil {
		return Stitch{}, Stitch{}, StitchLimits{}, fmt.Errorf("cgi: parse stitch: %w", err)
	}
	_ = json.Unmarshal(initial, &ini)

	var r struct {
		Stitch struct {
			Distance struct{ Min, Max float64 } `json:"distance"`
			XMove    struct{ Min, Max int }     `json:"stitchXMove"`
			YMove    struct{ Min, Max int }     `json:"stitchYMove"`
		} `json:"stitch"`
	}
	_ = json.Unmarshal(rng, &r)

	limits.Distance = r.Stitch.Distance
	limits.XMove = r.Stitch.XMove
	limits.YMove = r.Stitch.YMove
	return cur.Stitch, ini.Stitch, limits, nil
}

// SetStitch writes the stitch settings. Unlike a fisheye mode change this is
// a numeric adjustment and does not reboot the camera.
func (c *Client) SetStitch(ctx context.Context, channel int, s Stitch) error {
	return c.Set(ctx, "SetStitch", map[string]any{
		"stitch": map[string]any{
			"channel":     channel,
			"distance":    s.Distance,
			"stitchXMove": s.XMove,
			"stitchYMove": s.YMove,
		},
	})
}
