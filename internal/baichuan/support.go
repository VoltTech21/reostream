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

		// Item carries the per channel capabilities, and it is where most of
		// what distinguishes one model from another actually lives. The same
		// element is reused for smart home integrations, which have a name
		// and a version and no channel, so entries are told apart by whether
		// they carry a chnID.
		Item []SupportChannel `xml:"item"`
	} `xml:"Support"`
}

// SupportChannel is one channel's capabilities.
//
// The values are not all booleans. Several are bitmasks or version numbers:
// FishEye reads 3 on the fisheye here, AIType 3879, ISPCfg 195. Treat a
// nonzero value as "present, in some form" and do not read the magnitude as
// a count of anything without checking.
type SupportChannel struct {
	Name        string `xml:"name"`
	ChannelID   *int   `xml:"chnID"`
	PTZType     int    `xml:"ptzType"`
	PTZControl  int    `xml:"ptzControl"`
	PTZPreset   int    `xml:"ptzPreset"`
	PTZPatrol   int    `xml:"ptzPatrol"`
	PTZTattern  int    `xml:"ptzTattern"`
	AutoPT      int    `xml:"autoPt"`
	AutoFocus   int    `xml:"autoFocus"`
	ZFBacklash  int    `xml:"zfBacklash"`
	Battery     int    `xml:"battery"`
	BatAnalysis int    `xml:"batAnalysis"`
	NoAudio     int    `xml:"noAudio"`
	AudioVer    int    `xml:"audioVersion"`
	LEDCtrl     int    `xml:"ledCtrl"`
	ISPCfg      int    `xml:"ispCfg"`
	NewISPCfg   int    `xml:"newIspCfg"`
	OSDCfg      int    `xml:"osdCfg"`
	EncCtrl     int    `xml:"encCtrl"`
	Motion      int    `xml:"motion"`
	AIType      int    `xml:"aitype"`
	Snap        int    `xml:"snap"`
	VideoClip   int    `xml:"videoClip"`
	Timelapse   int    `xml:"timelapse"`
	Thumbnail   int    `xml:"thumbnail"`
	DynamicReso int    `xml:"dynamicReso"`
	RFCfg       int    `xml:"rfCfg"`
	FishEye     int    `xml:"fishEye"`
	BinoCfg     int    `xml:"binoCfg"`
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

// Channel returns the capabilities for one channel, or false if the camera
// did not report it.
func (s Support) Channel(id int) (SupportChannel, bool) {
	for _, it := range s.Support.Item {
		if it.ChannelID != nil && *it.ChannelID == id {
			return it, true
		}
	}
	return SupportChannel{}, false
}

// HasPTZ reports whether the camera has motors. ptzMode reads "none" on every
// fixed camera here.
func (s Support) HasPTZ() bool { return s.Support.PTZMode != "" && s.Support.PTZMode != "none" }
