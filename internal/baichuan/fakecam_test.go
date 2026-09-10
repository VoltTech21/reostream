package baichuan

import "testing"

// This file holds helpers that build synthetic wire bytes for scenarios no
// committed capture has ever recorded, such as a rejected login. They stay
// inside package baichuan (rather than in internal/fakecam, which is
// deliberately protocol-free) because building them needs BCCrypt and the
// header layout.

// walkMessages decodes every message in b, in order, stopping early if fn
// returns false. Unlike walkCapture in crypto_test.go, which reads a
// committed fixture off disk, this walks an arbitrary byte slice: tests use
// it on fakecam.Camera.Received() to recover what a client actually wrote
// to the wire.
func walkMessages(b []byte, fn func(h Header, body []byte) bool) {
	off := 0
	for off+20 <= len(b) {
		h, n, err := DecodeHeader(b[off:])
		if err != nil {
			return
		}
		end := off + n + int(h.MsgLen)
		if end > len(b) {
			return
		}
		if !fn(h, b[off+n:end]) {
			return
		}
		off = end
	}
}

// fixturePrefixLen returns the byte length of the first n messages of a
// committed fixture, so a test can truncate a real capture at a message
// boundary (mid-handshake, or right after login, or a few frames in)
// instead of an arbitrary byte offset that might split a header.
func fixturePrefixLen(t *testing.T, name string, n int) int {
	t.Helper()
	b := loadFixture(t, name)
	off := 0
	for i := 0; i < n; i++ {
		h, hn, err := DecodeHeader(b[off:])
		if err != nil {
			t.Fatalf("decode message %d of %s: %v", i, name, err)
		}
		off += hn + int(h.MsgLen)
	}
	return off
}

// buildMessage frames body under h, setting MsgLen from its length the same
// way Writer.Write does.
func buildMessage(h Header, body []byte) []byte {
	h.MsgLen = uint32(len(body))
	return append(h.Encode(), body...)
}

// synthesizeLoginFailureFixture builds the two messages a Conn reads during
// login: a real negotiation reply carrying nonce, then a login reply with an
// empty body.
//
// An empty body is the only shape login() currently treats as a failure
// (conn.go checks len(reply.XML) == 0). No committed capture shows what a
// real camera sends back for a rejected credential, so this drives that
// existing branch directly rather than inventing an XML error payload that
// has never actually been observed on the wire.
func synthesizeLoginFailureFixture(nonce string) []byte {
	plain := []byte(xmlHeader +
		`<body><Encryption version="1.1"><type>bc</type><nonce>` + nonce + `</nonce></Encryption></body>`)
	nonceReply := buildMessage(Header{
		MsgID:   MsgIDLogin,
		Class:   ClassModern20,
		EncByte: NegotiateByte,
		DirByte: DirReply,
	}, BCCrypt(0, plain))

	loginFailure := buildMessage(Header{
		MsgID:   MsgIDLogin,
		Class:   ClassZero,
		EncByte: 0xc8,
	}, nil)

	return append(nonceReply, loginFailure...)
}

// fakeReply is one AES-encrypted post-login message to append after a
// synthesized successful login.
type fakeReply struct {
	id   uint32
	body string
}

// synthesizeSessionFixture builds a successful login handshake, a
// DeviceInfo login reply carrying deviceInfoXML, and then replies, each
// AES-encrypted the way every message past login is (see login_s2c.bin,
// where the DeviceInfo login reply is itself the last BC-encrypted message
// and everything after it, AbilityInfo included, is AES).
//
// It exists because no committed capture asks a camera anything beyond what
// login gets for free: read helpers built on top of GetConfig need a
// fixture that answers a request this package chose to send, which a static
// capture cannot do.
func synthesizeSessionFixture(t *testing.T, nonce, password, deviceInfoXML string, replies ...fakeReply) []byte {
	t.Helper()
	plainNonce := xmlHeader + `<body><Encryption version="1.1"><type>md5</type><nonce>` + nonce + `</nonce></Encryption></body>`
	nonceReply := buildMessage(Header{
		MsgID:   MsgIDLogin,
		Class:   ClassModern20,
		EncByte: NegotiateByte,
		DirByte: DirReply,
	}, BCCrypt(0, []byte(plainNonce)))

	loginReply := buildMessage(Header{MsgID: MsgIDLogin, Class: ClassZero},
		BCCrypt(0, []byte(deviceInfoXML)))

	out := append(nonceReply, loginReply...)
	key := AESKey(nonce, password)
	for _, r := range replies {
		enc, err := AESEncrypt(key, []byte(r.body))
		if err != nil {
			t.Fatalf("encrypt reply %d: %v", r.id, err)
		}
		out = append(out, buildMessage(Header{MsgID: r.id, Class: ClassZero}, enc)...)
	}
	return out
}
