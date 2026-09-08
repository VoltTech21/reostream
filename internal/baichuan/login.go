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
// characters. The truncation is not a typo — a working client sends 31, and
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
