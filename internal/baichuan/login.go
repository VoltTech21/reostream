package baichuan

import (
	"crypto/md5"
	"encoding/xml"
	"fmt"
	"strings"
)

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>`

// hashCred renders the credential hash the camera expects: the uppercase hex
// MD5 of the credential concatenated with the login nonce, truncated to 31
// characters. The truncation is not a typo: a working client sends 31, and
// the camera compares 31.
func hashCred(value, nonce string) string {
	sum := md5.Sum([]byte(value + nonce))
	return strings.ToUpper(fmt.Sprintf("%x", sum))[:31]
}

type encryptionReply struct {
	XMLName xml.Name `xml:"body"`
	Enc     struct {
		Type  string `xml:"type"`
		Nonce string `xml:"nonce"`
	} `xml:"Encryption"`
}

// parseNonce pulls the login nonce out of the camera's negotiation reply.
func parseNonce(x []byte) (string, error) {
	var b encryptionReply
	if err := xml.Unmarshal(x, &b); err != nil {
		return "", fmt.Errorf("baichuan: parse nonce: %w", err)
	}
	if b.Enc.Nonce == "" {
		return "", fmt.Errorf("baichuan: no nonce in the negotiation reply")
	}
	return b.Enc.Nonce, nil
}

// loginBody is the modern login message, shaped to match a working client.
type loginBody struct {
	XMLName   xml.Name `xml:"body"`
	LoginUser struct {
		Version  string `xml:"version,attr"`
		UserName string `xml:"userName"`
		Password string `xml:"password"`
		UserVer  int    `xml:"userVer"`
	} `xml:"LoginUser"`
	LoginNet struct {
		Version string `xml:"version,attr"`
		Type    string `xml:"type"`
		UDPPort int    `xml:"udpPort"`
	} `xml:"LoginNet"`
}

// loginXML builds the login message for a nonce.
func loginXML(user, pass, nonce string) ([]byte, error) {
	var b loginBody
	b.LoginUser.Version = "1.1"
	b.LoginUser.UserName = hashCred(user, nonce)
	b.LoginUser.Password = hashCred(pass, nonce)
	b.LoginUser.UserVer = 1
	b.LoginNet.Version = "1.1"
	b.LoginNet.Type = "LAN"
	b.LoginNet.UDPPort = 0
	out, err := xml.Marshal(&b)
	if err != nil {
		return nil, fmt.Errorf("baichuan: marshal login: %w", err)
	}
	return append([]byte(xmlHeader), out...), nil
}

// previewBody starts or stops a video stream.
type previewBody struct {
	XMLName xml.Name `xml:"body"`
	Preview struct {
		Version    string `xml:"version,attr"`
		ChannelID  int    `xml:"channelId"`
		Handle     int    `xml:"handle"`
		StreamType string `xml:"streamType,omitempty"`
	} `xml:"Preview"`
}

func previewXML(channel, handle int, streamType string) ([]byte, error) {
	var b previewBody
	b.Preview.Version = "1.1"
	b.Preview.ChannelID = channel
	b.Preview.Handle = handle
	b.Preview.StreamType = streamType
	out, err := xml.Marshal(&b)
	if err != nil {
		return nil, fmt.Errorf("baichuan: marshal preview: %w", err)
	}
	return append([]byte(xmlHeader), out...), nil
}

// StreamKind names the three streams a camera serves. externStream is the
// "balanced" stream, which the Reolink apps do not expose.
const (
	StreamMain   = "mainStream"
	StreamSub    = "subStream"
	StreamExtern = "externStream"
)

// StreamID is the value the encryption-offset field carries for each stream.
func StreamID(stream string) byte {
	switch stream {
	case StreamSub:
		return 1
	case StreamExtern:
		return 4
	default:
		return 0
	}
}

// channelBody is the extension header that carries nothing but a channel,
// which is the whole request for several query messages.
type channelBody struct {
	XMLName   xml.Name `xml:"Extension"`
	Version   string   `xml:"version,attr"`
	ChannelID int      `xml:"channelId"`
}

func channelXML(channel int) ([]byte, error) {
	b := channelBody{Version: "1.1", ChannelID: channel}
	return marshalDoc(&b, "channel")
}

// cameraXMLHeader is the XML declaration the cameras themselves emit. The
// space before "?>" is theirs, and it is load bearing: the declaration in
// xmlHeader, which login has always used and which works, is one byte
// shorter, and a TalkConfig built with it is rejected with status 400.
const cameraXMLHeader = `<?xml version="1.0" encoding="UTF-8" ?>`

// marshalDoc renders a message body the way the cameras write their own: the
// declaration, then one element per line, then a trailing newline.
//
// This is not cosmetic, and it is not guesswork either. The dissector records
// a 104 byte extension and a 394 byte TalkConfig, and those two numbers only
// add up in exactly this form. A compact document is accepted for a
// TalkAbility query and rejected for a TalkConfig, so leniency varies by
// message and the safe thing is to match the camera byte for byte.
func marshalDoc(v any, what string) ([]byte, error) {
	out, err := xml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("baichuan: marshal %s: %w", what, err)
	}

	doc := make([]byte, 0, len(cameraXMLHeader)+len(out)+32)
	doc = append(doc, cameraXMLHeader...)
	doc = append(doc, '\n')
	// One element per line: break wherever one tag ends and the next begins,
	// which leaves text content sitting with its own tags.
	for i, c := range out {
		doc = append(doc, byte(c))
		if c == '>' && i+1 < len(out) && out[i+1] == '<' {
			doc = append(doc, '\n')
		}
	}
	return append(doc, '\n'), nil
}

// talkConfigBody negotiates a two-way audio session. The values are not
// invented: every camera surveyed here answered TalkAbility with exactly one
// audioConfig, adpcm at 16 kHz, 16 bit, mono, lengthPerEncoder 1024, and FDX
// as the only duplex mode. Ask the camera rather than assuming, since a model
// that offers something else will list it there.
type talkConfigBody struct {
	XMLName    xml.Name `xml:"body"`
	TalkConfig struct {
		Version         string `xml:"version,attr"`
		ChannelID       int    `xml:"channelId"`
		Duplex          string `xml:"duplex"`
		AudioStreamMode string `xml:"audioStreamMode"`
		AudioConfig     struct {
			AudioType        string `xml:"audioType"`
			SampleRate       int    `xml:"sampleRate"`
			SamplePrecision  int    `xml:"samplePrecision"`
			LengthPerEncoder int    `xml:"lengthPerEncoder"`
			SoundTrack       string `xml:"soundTrack"`
		} `xml:"audioConfig"`
	} `xml:"TalkConfig"`
}

func talkConfigXML(channel int, cfg TalkFormat) ([]byte, error) {
	var b talkConfigBody
	b.TalkConfig.Version = "1.1"
	b.TalkConfig.ChannelID = channel
	b.TalkConfig.Duplex = cfg.Duplex
	b.TalkConfig.AudioStreamMode = cfg.StreamMode
	b.TalkConfig.AudioConfig.AudioType = cfg.AudioType
	b.TalkConfig.AudioConfig.SampleRate = cfg.SampleRate
	b.TalkConfig.AudioConfig.SamplePrecision = cfg.SamplePrecision
	b.TalkConfig.AudioConfig.LengthPerEncoder = cfg.LengthPerEncoder
	b.TalkConfig.AudioConfig.SoundTrack = cfg.SoundTrack
	return marshalDoc(&b, "talk config")
}

// talkDataBody is the extension header on a message carrying talk audio.
type talkDataBody struct {
	XMLName    xml.Name `xml:"Extension"`
	Version    string   `xml:"version,attr"`
	BinaryData int      `xml:"binaryData"`
	ChannelID  int      `xml:"channelId"`
}

func talkDataXML(channel int) ([]byte, error) {
	b := talkDataBody{Version: "1.1", BinaryData: 1, ChannelID: channel}
	return marshalDoc(&b, "talk data")
}

// snapBody asks the camera for a still. fullFrame 0 asks for the encoded
// frame as the stream carries it; the reply names a file and its size, and
// the bytes follow in later messages.
type snapBody struct {
	XMLName xml.Name `xml:"body"`
	Snap    struct {
		Version      string `xml:"version,attr"`
		ChannelID    int    `xml:"channelId"`
		LogicChannel int    `xml:"logicChannel"`
		Time         int    `xml:"time"`
		FullFrame    int    `xml:"fullFrame"`
		StreamType   string `xml:"streamType"`
	} `xml:"Snap"`
}

func snapXML(channel int, stream string) ([]byte, error) {
	var b snapBody
	b.Snap.Version = "1.1"
	b.Snap.ChannelID = channel
	b.Snap.LogicChannel = channel
	b.Snap.Time = 0
	b.Snap.FullFrame = 0
	b.Snap.StreamType = stream
	return marshalDoc(&b, "snap")
}

// abilityQueryBody asks what a user may do. The token here is not the login
// token: it is the list of modules to report on, and the camera answers only
// for the ones named.
type abilityQueryBody struct {
	XMLName  xml.Name `xml:"Extension"`
	Version  string   `xml:"version,attr"`
	UserName string   `xml:"userName"`
	Token    string   `xml:"token"`
}

// abilityModules is every module the cameras here recognise. Asking for all
// of them costs one round trip and avoids a caller having to know the names.
const abilityModules = "system, network, alarm, record, video, image"

func abilityXML(user string) ([]byte, error) {
	b := abilityQueryBody{Version: "1.1", UserName: user, Token: abilityModules}
	return marshalDoc(&b, "ability query")
}

// heartBeatBody is an empty element. The request carries no parameters; the
// reply is what has content.
type heartBeatBody struct {
	XMLName   xml.Name `xml:"body"`
	HeartBeat struct {
		Version string `xml:"version,attr"`
	} `xml:"HeartBeat"`
}

func heartBeatXML() ([]byte, error) {
	var b heartBeatBody
	b.HeartBeat.Version = "1.1"
	return marshalDoc(&b, "heartbeat")
}
