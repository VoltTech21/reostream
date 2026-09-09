package rtsp

import (
	"errors"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// errNoADTS means an AAC frame did not carry a parseable ADTS header. These
// cameras always send ADTS framed AAC, so this is a truncated frame rather
// than a different encapsulation, and skipping it costs one frame.
var errNoADTS = errors.New("rtsp: AAC frame carries no ADTS header")

// adtsConfig derives the AudioSpecificConfig the SDP needs from an ADTS
// framed AAC frame.
//
// The configuration is not sent out of band by these cameras, the same as the
// video parameter sets, so it has to come out of the first frame that carries
// it.
func adtsConfig(data []byte) (*mpeg4audio.AudioSpecificConfig, error) {
	var pkts mpeg4audio.ADTSPackets
	if err := pkts.Unmarshal(data); err != nil {
		return nil, err
	}
	if len(pkts) == 0 {
		return nil, errNoADTS
	}
	p := pkts[0]
	return &mpeg4audio.AudioSpecificConfig{
		Type:          p.Type,
		SampleRate:    p.SampleRate,
		ChannelConfig: p.ChannelConfig,
	}, nil
}

// stripADTS returns the raw access units inside an ADTS framed AAC frame.
//
// RTP carries the access unit alone: the ADTS header repeats what the SDP
// already states, and a decoder handed both reads the header as audio data.
func stripADTS(data []byte) ([][]byte, error) {
	var pkts mpeg4audio.ADTSPackets
	if err := pkts.Unmarshal(data); err != nil {
		return nil, err
	}
	if len(pkts) == 0 {
		return nil, errNoADTS
	}
	out := make([][]byte, 0, len(pkts))
	for _, p := range pkts {
		out = append(out, p.AU)
	}
	return out, nil
}
