package control

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// Factory defaults, for the camera page's reset buttons.
//
// A CGI read with action 1 answers with the camera's current value, its
// factory default ("initial") and the valid range together, which is how
// this page learns a default without keeping a table of its own: every
// value offered here is one the camera itself reported. The curated
// settings are Baichuan documents, so each default is mapped onto the
// XPath the page edits. Only fields where that mapping is one to one are
// mapped. Day and night mode and the LED are not: the CGI API spells their
// values differently from the Baichuan document, and a reset that wrote a
// CGI spelling into a Baichuan field would be a guess.

// cgiCorner is the CGI API's name for each corner an overlay can sit in.
// The CGI API also has top and bottom centre, which the Baichuan corner
// table has no pair for; a camera whose default is centred gets no reset.
var cgiCorner = map[string]string{
	"Upper Left":  "top left",
	"Upper Right": "top right",
	"Lower Left":  "bottom left",
	"Lower Right": "bottom right",
}

// readDefaults asks the camera for the factory values of the curated fields
// it can map, keyed by each field's primary XPath. Any read that fails
// simply contributes nothing.
func readDefaults(ctx context.Context, c *cgi.Client) map[string]string {
	out := map[string]string{}

	if _, initial, _, err := c.Get(ctx, "GetImage", 0); err == nil {
		var v struct {
			Image map[string]json.Number `json:"Image"`
		}
		if json.Unmarshal(initial, &v) == nil {
			for cgiKey, xpath := range map[string]string{
				"bright":     "VideoInput/bright",
				"contrast":   "VideoInput/contrast",
				"saturation": "VideoInput/saturation",
			} {
				if n, ok := v.Image[cgiKey]; ok {
					out[xpath] = n.String()
				}
			}
		}
	}

	if _, initial, _, err := c.Get(ctx, "GetOsd", 0); err == nil {
		var v struct {
			Osd struct {
				OsdChannel *struct {
					Enable *int   `json:"enable"`
					Pos    string `json:"pos"`
				} `json:"osdChannel"`
				OsdTime *struct {
					Enable *int   `json:"enable"`
					Pos    string `json:"pos"`
				} `json:"osdTime"`
			} `json:"Osd"`
		}
		if json.Unmarshal(initial, &v) == nil {
			if ch := v.Osd.OsdChannel; ch != nil {
				if ch.Enable != nil {
					out["OsdChannelName/enable"] = fmt.Sprint(*ch.Enable)
				}
				if xy, ok := cornerPair(ch.Pos); ok {
					out["OsdChannelName/topLeftX"] = xy
				}
			}
			if t := v.Osd.OsdTime; t != nil {
				if t.Enable != nil {
					out["OsdDatetime/enable"] = fmt.Sprint(*t.Enable)
				}
				if xy, ok := cornerPair(t.Pos); ok {
					out["OsdDatetime/topLeftX"] = xy
				}
			}
		}
	}
	return out
}

// cornerPair turns a CGI position name into the "x,y" pair the position
// control posts.
func cornerPair(pos string) (string, bool) {
	label, ok := cgiCorner[pos]
	if !ok {
		return "", false
	}
	for _, c := range osdCorners() {
		if c.Label == label {
			return c.X + "," + c.Y, true
		}
	}
	return "", false
}
