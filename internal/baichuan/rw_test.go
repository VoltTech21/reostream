package baichuan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

func TestExplainStatusNamesTheTrap(t *testing.T) {
	// 421 is the one that misleads. It reads like "this model does not
	// support that" and it means the message was built wrong: a config
	// write is two sections, and sent as one a camera answers 421 and
	// changes nothing. The same 421 came back from the mis-numbered
	// heartbeat, where the feature was fine and the id was wrong.
	got := ExplainStatus(StatusWrongShape)
	for _, want := range []string{"two section", "not"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ExplainStatus(421) = %q, which does not mention %q", got, want)
		}
	}
	if got := ExplainStatus(StatusNotImplemented); !strings.Contains(got, "does not implement") {
		t.Fatalf("ExplainStatus(405) = %q", got)
	}
	if got := ExplainStatus(200); !strings.Contains(got, "accepted") {
		t.Fatalf("ExplainStatus(200) = %q", got)
	}
}

// statusReply is fakeReply (see fakecam_test.go) plus an explicit status
// code. fakeReply always answers with status 0, and ReadConfig's contract
// includes handing back whatever status the camera actually sent.
type statusReply struct {
	id     uint32
	status int16
	body   string
}

// synthesizeSessionFixtureWithStatus is synthesizeSessionFixture with a
// status code encoded into each reply's header, for tests that need to
// observe the status ReadConfig hands back rather than just its body.
func synthesizeSessionFixtureWithStatus(t *testing.T, nonce, password, deviceInfoXML string, replies ...statusReply) []byte {
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
		out = append(out, buildMessage(Header{
			MsgID:   r.id,
			Class:   ClassZero,
			EncByte: byte(uint16(r.status) & 0xff),
			DirByte: byte(uint16(r.status) >> 8),
		}, enc)...)
	}
	return out
}

// ReadConfig is requestConfig's exported twin: this pins the round trip
// against a fake camera, and that a reply for a different message id (a
// ping in flight, for instance) does not get mistaken for the answer.
func TestReadConfigReturnsTheFixtureAndIgnoresOtherReplies(t *testing.T) {
	const nonce = "probe-test-nonce-0000000000"
	const wantXML = xmlHeader + `<body>
<Osd version="1.1">
<channel>0</channel>
</Osd>
</body>`

	cam := fakecam.New(t, synthesizeSessionFixtureWithStatus(t, nonce, "", testDeviceInfoXML,
		statusReply{id: MsgIDHeartbeat, status: 200, body: ""},
		statusReply{id: MsgIDOsdGet, status: 200, body: wantXML},
	))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	xml, status, err := ReadConfig(ctx, conn, MsgIDOsdGet)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(xml) != wantXML {
		t.Errorf("xml = %q, want %q", xml, wantXML)
	}
}
