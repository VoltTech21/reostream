package baichuan

import (
	"encoding/xml"
	"fmt"
)

// Support is the camera's own account of its hardware, from message 199.
//
// This is the capability source to trust. The other two lists each fail in a
// way this one does not: the HTTP GetAbility list varies by model but reports
// `talk` on cameras with no speaker, and Baichuan's AbilityInfo is a per-user
// permission list that reads identically across every model.
//
// Checked against behaviour established the slow way on eight cameras:
// AudioTalk is 1 on exactly the two that make a sound and 0 on the six that
// accept a talk session once and then refuse forever, and NoExternStream is 1
// on exactly the one camera that answers a balanced stream request with
// silence. Both would have been one query instead of an afternoon.
type Support struct {
	XMLName xml.Name `xml:"body"`
	Support struct {
		IOInputPortNum  int    `xml:"IOInputPortNum"`
		IOOutputPortNum int    `xml:"IOOutputPortNum"`
		DiskNum         int    `xml:"diskNum"`
		ChannelNum      int    `xml:"channelNum"`
		AudioNum        int    `xml:"audioNum"`
		PTZMode         string `xml:"ptzMode"`
		PTZCfg          int    `xml:"ptzCfg"`
		B485            int    `xml:"B485"`
		WiFi            int    `xml:"wifi"`
		WiFiVersion     int    `xml:"wifiVersion"`
		GPS             int    `xml:"gps"`
		PowerSavingCfg  int    `xml:"powerSavingCfg"`
		RFVersion       int    `xml:"rfVersion"`
		AudioTalk       int    `xml:"audioTalk"`
		AudioAlarm      int    `xml:"audioAlarm"`
		AudioCfg        int    `xml:"audioCfg"`
		NoExternStream  int    `xml:"noExternStream"`
		RTSP            int    `xml:"rtsp"`
		ONVIF           int    `xml:"onvif"`
		RTMP            int    `xml:"rtmp"`
		Record          int    `xml:"record"`
		StorageMode     int    `xml:"storagemode"`
		Reboot          int    `xml:"reboot"`
		Upgrade         int    `xml:"upgrade"`
	} `xml:"Support"`
}

// ParseSupport reads a Support reply.
func ParseSupport(x []byte) (Support, error) {
	var s Support
	if err := xml.Unmarshal(x, &s); err != nil {
		return Support{}, fmt.Errorf("baichuan: parse support: %w", err)
	}
	return s, nil
}

// CanTalk reports whether the camera has a speaker to talk through.
//
// Worth checking before opening a talk session rather than after: a camera
// without one accepts the configuration, answers 200, makes no sound, and
// then refuses every later session until it reboots.
func (s Support) CanTalk() bool { return s.Support.AudioTalk != 0 }

// HasExternStream reports whether the camera serves the balanced stream.
func (s Support) HasExternStream() bool { return s.Support.NoExternStream == 0 }

// HasPTZ reports whether the camera has motors. ptzMode reads "none" on every
// fixed camera here.
func (s Support) HasPTZ() bool { return s.Support.PTZMode != "" && s.Support.PTZMode != "none" }
