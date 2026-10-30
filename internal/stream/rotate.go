package stream

import (
	"bytes"
	"strconv"
	"strings"

	mch264 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mch265 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Turning a sideways picture without re-encoding it.
//
// The 16 MP dual-lens boards (Duo 3 and Elite Pro Floodlight on the NT17
// chip) cannot encode a 7680-wide frame, so at full resolution they encode
// it 2160 wide by 7680 tall, turned 90 degrees clockwise, and tell the
// Reolink app at login (DeviceInfo needRotate) to turn it back. Re-encoding
// 16 MP to fix that is out of the question here; instead each keyframe gets
// an H.265 display orientation SEI (payload type 47), the standard's own way
// of saying "show this turned". ffmpeg honors it on decode (6.1 and the 7.0
// Frigate ships both checked), so Frigate's detector, its snapshots and VLC
// see the picture upright. A stream copied into an MP4 does not carry it as
// a container rotation, so recordings may still need the recorder told.

// rotationFor is how many degrees anticlockwise this stream's picture should
// be shown turned: the camera's rotate setting when there is one, otherwise
// what the camera said at login. needRotate on its own does not mean the
// picture is sideways: an NT13 Duo 3 whose picture is upright sends "1"
// too (seen 10-27), which is why Run only acts on it for a picture that
// really is taller than wide, judged from the keyframe itself (pictureSize). A camera that asks is assumed to mean the 90
// degrees the NT17 boards were reported to need (picture turned clockwise,
// left side up), unless it names 90, 180 or 270 itself.
func rotationFor(cfg Config, deviceInfo []byte) (deg int, fromCamera bool, raw string) {
	if cfg.Rotate != nil {
		return *cfg.Rotate, false, ""
	}
	di, err := baichuan.ParseDeviceInfo(deviceInfo)
	if err != nil {
		return 0, false, ""
	}
	raw = strings.TrimSpace(di.DeviceInfo.NeedRotate)
	switch raw {
	case "", "0":
		return 0, true, raw
	case "90", "180", "270":
		n, _ := strconv.Atoi(raw)
		return n, true, raw
	}
	return 90, true, raw
}

// orientationSEI is a prefix SEI NAL carrying one display orientation
// message: no flips, anticlockwise_rotation in 1/65536ths of a turn, shown
// until canceled (repetition period 1, coded ue(v) as a single 1 bit).
func orientationSEI(deg int) []byte {
	rot := uint32(deg%360) * 65536 / 360
	// cancel=0, hor_flip=0, ver_flip=0, rotation(16), period ue(0)="1",
	// extension_flag=0, then the rbsp stop bit and zero padding.
	var bits uint64
	n := 0
	put := func(v uint64, w int) { bits = bits<<w | v; n += w }
	put(0, 3)
	put(uint64(rot&0xFFFF), 16)
	put(1, 1)
	put(0, 1)
	put(1, 1)
	for n%8 != 0 {
		put(0, 1)
	}
	payload := make([]byte, n/8)
	for i := range payload {
		payload[i] = byte(bits >> (8 * (len(payload) - 1 - i)))
	}
	// NAL header: type 39 (prefix SEI), layer 0, temporal id 1.
	sei := []byte{0, 0, 0, 1, 0x4E, 0x01, 47, byte(len(payload))}
	sei = append(sei, payload...)
	return append(sei, 0x80)
}

// withOrientation returns an H.265 access unit with the orientation SEI
// placed just before its first picture slice, which is where a prefix SEI
// belongs: after the parameter sets, before the picture it describes.
// Anything it cannot place it leaves exactly as it was.
func withOrientation(data []byte, deg int) []byte {
	sc := []byte{0, 0, 1}
	for i := 0; i+3 < len(data); {
		j := bytes.Index(data[i:], sc)
		if j < 0 {
			break
		}
		at := i + j
		if at+3 >= len(data) {
			break
		}
		if typ := (data[at+3] >> 1) & 0x3F; typ < 32 {
			// A slice: back up over a four-byte start code's leading zero.
			if at > 0 && data[at-1] == 0 {
				at--
			}
			out := make([]byte, 0, len(data)+16)
			out = append(out, data[:at]...)
			out = append(out, orientationSEI(deg)...)
			return append(out, data[at:]...)
		}
		i = at + 3
	}
	return data
}

func isHEVC(codec string) bool {
	return strings.EqualFold(codec, "h265") || strings.EqualFold(codec, "hevc")
}

// pictureSize reads the coded picture's width and height from the sequence
// parameter set in a keyframe, which is the size the camera actually
// encoded. The media info packet is no use for this: the NT17 firmware
// fills it from the configured size, 7680x2160, while the frames it encodes
// are 2160x7680 (emulated v3.2.0.5770, 2026-10-29), so a picture that is
// sideways never looks it there.
func pictureSize(codec string, data []byte) (w, h int, ok bool) {
	var au mch264.AnnexB
	if err := au.Unmarshal(data); err != nil {
		return 0, 0, false
	}
	for _, nalu := range au {
		if len(nalu) < 2 {
			continue
		}
		if isHEVC(codec) {
			if mch265.NALUType((nalu[0]>>1)&0x3F) != mch265.NALUType_SPS_NUT {
				continue
			}
			var sps mch265.SPS
			if sps.Unmarshal(nalu) != nil {
				return 0, 0, false
			}
			return sps.Width(), sps.Height(), true
		}
		if mch264.NALUType(nalu[0]&0x1F) != mch264.NALUTypeSPS {
			continue
		}
		var sps mch264.SPS
		if sps.Unmarshal(nalu) != nil {
			return 0, 0, false
		}
		return sps.Width(), sps.Height(), true
	}
	return 0, 0, false
}
