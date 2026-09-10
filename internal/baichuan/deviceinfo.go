package baichuan

import (
	"encoding/xml"
	"fmt"
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
		AudioNum    int    `xml:"audioNum"`
		Resolution  struct {
			Name   string `xml:"resolutionName"`
			Width  int    `xml:"width"`
			Height int    `xml:"height"`
		} `xml:"resolution"`
	} `xml:"DeviceInfo"`
}

// ParseDeviceInfo reads the document Conn.DeviceInfo returns.
func ParseDeviceInfo(x []byte) (DeviceInfo, error) {
	var d DeviceInfo
	if err := xml.Unmarshal(x, &d); err != nil {
		return DeviceInfo{}, fmt.Errorf("baichuan: parse device info: %w", err)
	}
	return d, nil
}
