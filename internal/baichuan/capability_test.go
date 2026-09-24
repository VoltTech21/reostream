package baichuan

import "testing"

// The values here are what two real cameras on the fleet answered, taken
// when the control page was found showing a floodlight on a camera that has
// no floodlight.
//
// The measurement that matters is the negative one: GetWhiteLed returns a
// full WhiteLed document on both cameras, identical but for an unrelated AI
// flag, so the page cannot tell them apart from the light's own document.
// ledCtrl can: 0 against 38.
func TestFloodlightAndFishEyeComeFromTheSupportBlock(t *testing.T) {
	// fisheye: dewarping, no light.
	fisheye := Support{}
	fisheye.Support.Item = []SupportChannel{{ChannelID: intp(0), LEDCtrl: 0, FishEye: 3}}
	// pano: a real white light, two lenses stitched rather than dewarped.
	pano := Support{}
	pano.Support.Item = []SupportChannel{{ChannelID: intp(0), LEDCtrl: 38, FishEye: 0}}

	if fisheye.HasFloodlight(0) {
		t.Error("the fisheye reports a floodlight; ledCtrl is 0 on it")
	}
	if !pano.HasFloodlight(0) {
		t.Error("the pano reports no floodlight; ledCtrl is 38 on it")
	}
	if !fisheye.HasFishEye(0) {
		t.Error("the fisheye reports no dewarping; fishEye is 3 on it")
	}
	if pano.HasFishEye(0) {
		t.Error("the pano reports dewarping; fishEye is 0 on it")
	}

	// A channel that was never reported must not answer yes to either: an
	// unknown camera gets no control, rather than a control that fails on
	// the first press.
	var empty Support
	if empty.HasFloodlight(0) || empty.HasFishEye(0) {
		t.Error("a support block with no channels claimed a capability")
	}
}

func intp(v int) *int { return &v }
