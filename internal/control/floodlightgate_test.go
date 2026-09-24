package control

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// The page drew a floodlight control on the fisheye, which has no
// floodlight, because it rendered whatever GetWhiteLed returned and
// GetWhiteLed answers on every camera. The Support block's ledCtrl is what
// actually differs: 0 on the fisheye, 38 on the pano.
//
// The three cases below are the whole rule, and the third is the one worth
// keeping honest: an unread support block is not a camera saying no.
func TestTheFloodlightIsHiddenOnlyWhenTheCameraSaysItHasNone(t *testing.T) {
	saysNo := &baichuan.Support{}
	saysNo.Support.Item = []baichuan.SupportChannel{{ChannelID: intPtr(0), LEDCtrl: 0}}

	saysYes := &baichuan.Support{}
	saysYes.Support.Item = []baichuan.SupportChannel{{ChannelID: intPtr(0), LEDCtrl: 38}}

	for _, c := range []struct {
		name string
		sup  *baichuan.Support
		want bool
	}{
		{"camera reports no light", saysNo, false},
		{"camera reports a light", saysYes, true},
		{"support could not be read", nil, true},
	} {
		got := c.sup == nil || c.sup.HasFloodlight(0)
		if got != c.want {
			t.Errorf("%s: floodlight shown = %v, want %v", c.name, got, c.want)
		}
	}
}

func intPtr(v int) *int { return &v }
