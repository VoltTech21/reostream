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
// Checked against behavior established the slow way on eight cameras:
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
		// FTP is the camera's FTP feature bitmask: nonzero on a camera with
		// FTP upload, 0 on builds that ship with it switched off (the
		// compact E1/E320/E330 and NVC-D12M firmware, support_ftp="0" in
		// their dvr.xml), which refuse every FTP request with 400. A pointer
		// so a camera that leaves the element out is not read as a no.
		FTP *int `xml:"ftp"`

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

// HasFloodlight reports whether channel id has a white light to drive.
//
// GetWhiteLed cannot answer this. Measured on this fleet, a camera with no
// light and one with a real one return the same WhiteLed document, mode,
// state, brightness and schedule alike, differing only in an unrelated AI
// flag. So a page that renders whatever GetWhiteLed returns shows a
// floodlight control on cameras that have no floodlight, which is what
// happened.
//
// ledCtrl answers it, but as bits, not as zero or not: 0 on the fisheye,
// 38 (bits 1, 2, 5) on the pano and the TrackMix, 4111 (bits 0-3, 12) on
// the Argus PT Ultra, all three with a white light, and 64 (bit 6 alone)
// on the RLC-810A, which has none and was being offered floodlight
// controls (click test, 10-27). Bits 1 and 2 are what every camera here
// with a white light has and the others lack.
func (s Support) HasFloodlight(id int) bool {
	ch, ok := s.Channel(id)
	return ok && ch.LEDCtrl&0x6 != 0
}

// LacksFTP reports whether the camera says outright that it has no FTP
// upload. Only a positive no counts: a Support block without the element
// says nothing either way.
func (s Support) LacksFTP() bool {
	return s.Support.FTP != nil && *s.Support.FTP == 0
}

// HasFishEye reports whether channel id has the dewarping view modes that
// GetFishEye and SetFishEye drive. Measured: 3 on the fisheye, 0 on the
// pano, which has two lenses stitched instead.
func (s Support) HasFishEye(id int) bool {
	ch, ok := s.Channel(id)
	return ok && ch.FishEye != 0
}

// HasStitch reports whether channel id has two lenses whose seam GetStitch
// and SetStitch adjust.
//
// Measured on this fleet, binoCfg is exactly the inverse of fishEye: 1 on
// the pano and 0 on the fisheye, against 0 and 3 for fishEye. That is the
// physical fact behind it -- one sensor behind one lens has no seam, and
// two lenses have nothing to dewarp -- so the two controls are never both
// offered, and neither is ever offered on the six ordinary cameras.
func (s Support) HasStitch(id int) bool {
	ch, ok := s.Channel(id)
	return ok && ch.BinoCfg != 0
}
