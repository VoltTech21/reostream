package baichuan

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// DeviceInfo is the document a camera sends as its login reply on success,
// before it names anything about a stream or asks a config question of its
// own. Captured whole on a real camera (internal/baichuan/testdata,
// login_s2c.bin, message 1): a firmware version, a device type ("ipc"), the
// channel and audio counts also carried in Support, and the camera's own
// top resolution.
//
// It carries no marketing model name ("RLC-810A" and similar never appear
// anywhere in it), so nothing here should be presented as one.
type DeviceInfo struct {
	XMLName    xml.Name `xml:"body"`
	DeviceInfo struct {
		FirmVersion string `xml:"firmVersion"`
		Type        string `xml:"type"`     // "ipc" on every camera captured so far
		TypeInfo    string `xml:"typeInfo"` // "IPC"; same information, differently cased
		ChannelNum  int    `xml:"channelNum"`
		// NeedRotate is set by firmware that encodes a picture sideways
		// and expects the client to turn it back (the 16 MP NT17 boards'
		// full resolution). Every build sends the element; only those set
		// it non-zero. The value's meaning beyond "non-zero" has not been
		// seen on the wire yet.
		NeedRotate string `xml:"needRotate"`
		AudioNum   int    `xml:"audioNum"`
		Resolution struct {
			Name   string `xml:"resolutionName"`
			Width  int    `xml:"width"`
			Height int    `xml:"height"`
		} `xml:"resolution"`
	} `xml:"DeviceInfo"`
}

// SecondLens reports a camera with a second lens on channel 1: the
// TrackMix's zoom lens (and the other two-lens trackers, the TrackFlex and
// the Omvi 3i, which report the same way). It is in the login reply, so
// knowing it costs nothing, where asking channel 1 for video takes one of
// the camera's two main stream slots and waits on an answer a camera with
// its slots taken never sends.
//
// An NVR or Home Hub reports a channel per camera it holds, so only a
// device that calls itself a camera ("ipc", "fisheye_ipc") counts. The
// dual-lens panoramas (Duo 2, Duo 3) stitch both lenses into one picture
// and report one channel (pano, 10-27 sweep), so they are not counted.
func (d DeviceInfo) SecondLens() bool {
	t := strings.ToLower(d.DeviceInfo.Type + " " + d.DeviceInfo.TypeInfo)
	return d.DeviceInfo.ChannelNum >= 2 && strings.Contains(t, "ipc")
}

// ParseDeviceInfo reads the document Conn.DeviceInfo returns.
func ParseDeviceInfo(x []byte) (DeviceInfo, error) {
	var d DeviceInfo
	if err := xml.Unmarshal(x, &d); err != nil {
		return DeviceInfo{}, fmt.Errorf("baichuan: parse device info: %w", err)
	}
	return d, nil
}
