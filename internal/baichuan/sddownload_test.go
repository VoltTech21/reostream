package baichuan

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// A TrackMix sends a recording as one byte stream cut into messages
// wherever its buffer filled, each marked as carrying binary data, so a
// keyframe often starts in one message and ends in the next. Reading each
// message as a new packet threw those keyframes away and the recording
// came back with 0 frames (#21).
func TestDownloadKeepsAFrameSplitAcrossMessages(t *testing.T) {
	seal := BCCrypt
	nonce := xmlHeader + `<body><Encryption version="1.1"><type>md5</type><nonce>0123456789ABCDEF</nonce></Encryption></body>`
	fixture := buildMessage(Header{MsgID: MsgIDLogin, Class: ClassModern20, EncByte: 0x01, DirByte: DirReply}, seal(0, []byte(nonce)))
	fixture = append(fixture, buildMessage(Header{MsgID: MsgIDLogin, Class: ClassZero, EncByte: 0xc8}, seal(0, []byte(testDeviceInfoXML)))...)

	picture := append([]byte{0, 0, 0, 1, 0x40, 0x01}, bytes.Repeat([]byte{0xab}, 200)...)
	stream := buildFrame(magicIFrame, 32, "H265", 1000, picture)
	stream = append(stream, buildFrame(magicPFrame, 24, "H265", 2000, []byte{0, 0, 0, 1, 0x02, 0x01, 0xcd})...)
	for len(stream)%8 != 0 {
		stream = append(stream, 0)
	}
	ext := seal(0, []byte(xmlHeader+`<Extension version="1.1"><binaryData>1</binaryData></Extension>`))
	for _, part := range [][]byte{stream[:100], stream[100:]} {
		h := Header{MsgID: msgDownload, Class: ClassModern24, EncByte: 0xc8, PayloadOff: uint32(len(ext))}
		fixture = append(fixture, buildMessage(h, append(append([]byte{}, ext...), part...))...)
	}
	fixture = append(fixture, buildMessage(Header{MsgID: msgDownload, Class: ClassModern24, EncByte: 0x2c, DirByte: 0x01}, nil)...)

	cam := fakecam.New(t, fixture)
	conn, err := Dial(context.Background(), cam.Addr(), Options{Username: "admin", Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	var kinds []FrameKind
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	end, err := conn.Download(ctx, time.Now(), time.Now(), false, time.Second, func(f Frame) error {
		kinds = append(kinds, f.Kind)
		return nil
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if end != errDownloadDone.Error() {
		t.Errorf("end %q", end)
	}
	if len(kinds) != 2 || kinds[0] != FrameIFrame || kinds[1] != FramePFrame {
		t.Fatalf("frames %v, want the I frame and the P frame", kinds)
	}
}
