package baichuan

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// The negotiation reply names the camera's cipher in byte 16 when byte 17 is
// DirReply. These are the answers v3.0.0.136 gave to each request on
// emulation (2026-11-02): it echoes 0x00, 0x01 and 0x03, and answers 0x12 with
// 0x02. Anything not known keeps the AES every session used before.
func TestNegotiatedCipher(t *testing.T) {
	for _, tc := range []struct {
		enc, dir byte
		want     Cipher
	}{
		{0x12, DirReply, CipherAES},
		{0x02, DirReply, CipherAES},
		{0x03, DirReply, CipherAES},
		{0x01, DirReply, CipherBC},
		{0x00, DirReply, CipherNone},
		{0x01, 0x00, CipherAES},
		{0xc8, 0x00, CipherAES},
	} {
		if got := negotiatedCipher(Header{EncByte: tc.enc, DirByte: tc.dir}); got != tc.want {
			t.Errorf("reply %02x %02x: %v, want %v", tc.enc, tc.dir, got, tc.want)
		}
	}

	// The committed capture is current firmware, which must stay AES.
	h, _, err := DecodeHeader(loadFixture(t, "login_s2c.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := negotiatedCipher(h); got != CipherAES {
		t.Errorf("login_s2c.bin negotiates %v, want aes", got)
	}
}

// cipherSessionFixture is a camera that answers the negotiation with enc and
// then seals everything with seal: the nonce, the DeviceInfo login reply, and
// one Preview reply carrying an AAC packet as its payload.
func cipherSessionFixture(enc byte, seal func(off uint32, b []byte) []byte) []byte {
	nonce := xmlHeader + `<body><Encryption version="1.1"><type>md5</type><nonce>0123456789ABCDEF</nonce></Encryption></body>`
	out := buildMessage(Header{MsgID: MsgIDLogin, Class: ClassModern20, EncByte: enc, DirByte: DirReply},
		seal(0, []byte(nonce)))
	out = append(out, buildMessage(Header{MsgID: MsgIDLogin, Class: ClassZero, EncByte: 0xc8},
		seal(0, []byte(testDeviceInfoXML)))...)

	off := EncOffsetFor(0, StreamID(StreamMain), 1, 0)
	ext := seal(off, []byte(xmlHeader+`<Extension version="1.1"><binaryData>1</binaryData></Extension>`))
	h := Header{MsgID: MsgIDVideo, Class: ClassModern24, EncOffset: off, EncByte: 0xc8, PayloadOff: uint32(len(ext))}
	pkt := append(append([]byte{}, magicAAC...), 3, 0, 3, 0, 0xff, 0xf1, 0x50)
	return append(out, buildMessage(h, append(ext, pkt...))...)
}

// streamUnderCipher logs in to a camera serving fixture, starts the main
// stream and returns the first message, what the camera received, and the
// cipher the session settled on.
func streamUnderCipher(t *testing.T, fixture []byte) (Message, []byte, Cipher) {
	t.Helper()
	cam := fakecam.New(t, fixture)
	conn, err := Dial(context.Background(), cam.Addr(), Options{Username: "admin", Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.StartVideo(StreamMain); err != nil {
		t.Fatalf("start video: %v", err)
	}
	var m Message
	select {
	case m = <-conn.Messages():
	case <-time.After(2 * time.Second):
		t.Fatal("no message after the Preview request")
	}
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for cam.Closes() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return m, cam.Received(), conn.Cipher()
}

// sentPreview returns the client's Preview request opened with open.
func sentPreview(received []byte, open func(off uint32, b []byte) []byte) string {
	var got string
	walkMessages(received, func(h Header, body []byte) bool {
		if h.MsgID == MsgIDVideo && len(body) > 0 {
			got = string(open(h.EncOffset, body))
			return false
		}
		return true
	})
	return got
}

// The C1 Pro and RLC-410 on v2.0.0.x answer the negotiation with 0x01 and
// keep the BC cipher after login. A Preview sealed with AES goes unanswered
// there, so the stream never starts (#23); v3.0.0.136 asked for BC answers
// the same way and streams normally on a BC sealed Preview.
func TestCameraThatNegotiatesBCGetsBC(t *testing.T) {
	m, received, c := streamUnderCipher(t, cipherSessionFixture(0x01, BCCrypt))
	if c != CipherBC {
		t.Fatalf("session cipher %v, want bc", c)
	}
	if p := sentPreview(received, BCCrypt); !strings.Contains(p, "<Preview") {
		t.Fatalf("Preview is not BC sealed: %q", p)
	}
	if !bytes.Contains(m.XML, []byte("<binaryData>1</binaryData>")) {
		t.Fatalf("reply not read as BC: %q", m.XML)
	}
	if len(m.Payload) != 11 || !bytes.HasPrefix(m.Payload, magicAAC) {
		t.Fatalf("payload %x", m.Payload)
	}
}

// A camera that answers 0x00 sends everything in the clear from the
// negotiation reply on, and refuses a BC sealed login with status 400
// (v3.0.0.136, emulated 2026-11-02).
func TestCameraThatNegotiatesNoCipherGetsPlaintext(t *testing.T) {
	plain := func(_ uint32, b []byte) []byte { return b }
	m, received, c := streamUnderCipher(t, cipherSessionFixture(0x00, plain))
	if c != CipherNone {
		t.Fatalf("session cipher %v, want none", c)
	}
	var login string
	walkMessages(received, func(h Header, body []byte) bool {
		if h.MsgID == MsgIDLogin && len(body) > 0 {
			login = string(body)
			return false
		}
		return true
	})
	if !strings.Contains(login, "<LoginUser") {
		t.Fatalf("login not sent in the clear: %q", login)
	}
	if p := sentPreview(received, plain); !strings.Contains(p, "<Preview") {
		t.Fatalf("Preview not sent in the clear: %q", p)
	}
	if !bytes.Contains(m.XML, []byte("<binaryData>1</binaryData>")) {
		t.Fatalf("reply not read in the clear: %q", m.XML)
	}
}
