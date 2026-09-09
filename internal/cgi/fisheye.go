package cgi

import (
	"encoding/json"
	"fmt"
)

// FishEyeMode is what the camera does to the circular image before encoding
// it. All four produce the same frame size; modes 1 to 3 are dewarped by the
// camera, so a client cropping regions out of the raw circle and correcting
// them itself is redoing work the camera will do properly.
type FishEyeMode int

const (
	FishEyeRaw      FishEyeMode = 0 // the circle, uncorrected
	FishEyePanorama FishEyeMode = 1 // the circle unwrapped into a flat band
	FishEyeQuad     FishEyeMode = 2 // four dewarped sub views in a 2x2 grid
	FishEyeDual     FishEyeMode = 3 // two dewarped halves, stacked
)

func (m FishEyeMode) String() string {
	switch m {
	case FishEyeRaw:
		return "raw fisheye circle"
	case FishEyePanorama:
		return "panorama"
	case FishEyeQuad:
		return "quad, 2x2 dewarped views"
	case FishEyeDual:
		return "dual, two stacked halves"
	}
	return fmt.Sprintf("unknown mode %d", int(m))
}

// FishEye is the camera's current view configuration.
type FishEye struct {
	ImageType     FishEyeMode `json:"imageType"`
	InstallType   int         `json:"installType"`
	RotationAngle int         `json:"rotationAngle"`
}

// GetFishEye reads the fisheye view configuration.
func (c *Client) GetFishEye(channel int) (FishEye, error) {
	value, _, _, err := c.Get("GetFishEye", channel)
	if err != nil {
		return FishEye{}, err
	}
	var v struct {
		FishEye FishEye `json:"FishEye"`
	}
	if err := json.Unmarshal(value, &v); err != nil {
		return FishEye{}, fmt.Errorf("cgi: parse fisheye: %w", err)
	}
	return v.FishEye, nil
}

// SetFishEye changes the view mode.
//
// **This reboots the camera.** That is normal for this setting rather than a
// fault, and it has consequences worth planning for: the HTTP API returns 502
// or nothing for roughly 15 to 20 seconds, the auth token is invalidated so
// anything afterwards needs a fresh login, and any recorder consuming the
// camera loses the stream and reconnects. A fixed sleep is the wrong tool for
// waiting it out; poll until the camera answers.
//
// It also invalidates any motion mask or detection zone drawn against the
// previous geometry, because those coordinates are normalised to the frame.
// A fisheye with zones drawn on the circular view will have them pointing at
// the wrong places after a switch to quad or dual.
func (c *Client) SetFishEye(channel int, f FishEye) error {
	return c.Set("SetFishEye", map[string]any{
		"FishEye": map[string]any{
			"channel":       channel,
			"imageType":     int(f.ImageType),
			"installType":   f.InstallType,
			"rotationAngle": f.RotationAngle,
		},
	})
}
