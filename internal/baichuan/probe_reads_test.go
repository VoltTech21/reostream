package baichuan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

const testDeviceInfoXML = xmlHeader + `<body>
<DeviceInfo version="1.1">
<firmVersion>00000000983040</firmVersion>
<type>ipc</type>
<typeInfo>IPC</typeInfo>
<channelNum>1</channelNum>
<audioNum>1</audioNum>
<resolution>
<resolutionName>3840*2160</resolutionName>
<width>3840</width>
<height>2160</height>
</resolution>
</DeviceInfo>
</body>`

// Dial reads DeviceInfo for free as part of login (see docs/protocol.md's
// handshake table): this pins that Conn actually keeps it, since nothing
// else in this package surfaces it otherwise.
func TestDialCapturesDeviceInfo(t *testing.T) {
	const nonce = "probe-test-nonce-0000000000"
	cam := fakecam.New(t, synthesizeSessionFixture(t, nonce, "", testDeviceInfoXML))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	di, err := ParseDeviceInfo(conn.DeviceInfo())
	if err != nil {
		t.Fatalf("parse device info: %v", err)
	}
	if di.DeviceInfo.TypeInfo != "IPC" {
		t.Errorf("TypeInfo = %q, want IPC", di.DeviceInfo.TypeInfo)
	}
	if di.DeviceInfo.Resolution.Name != "3840*2160" {
		t.Errorf("Resolution.Name = %q, want 3840*2160", di.DeviceInfo.Resolution.Name)
	}
}

// The whole point of ErrUnauthorised: a caller has to be able to name a
// rejected credential, not just see that Dial failed.
func TestDialWrapsErrUnauthorisedOnAnEmptyLoginReply(t *testing.T) {
	const nonce = "doc-example-0000000000000000"
	cam := fakecam.New(t, synthesizeLoginFailureFixture(nonce))

	_, err := Dial(context.Background(), cam.Addr(), Options{Username: "admin", Password: "wrong"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrUnauthorised) {
		t.Errorf("err = %v, want it to wrap ErrUnauthorised", err)
	}
}

func TestGetSupportReadsTheSupportBlock(t *testing.T) {
	const nonce = "probe-test-nonce-0000000000"
	cam := fakecam.New(t, synthesizeSessionFixture(t, nonce, "", testDeviceInfoXML,
		fakeReply{id: ConfigMessages["support"], body: string(supportXML("1", "0"))}))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sup, err := GetSupport(ctx, conn)
	if err != nil {
		t.Fatalf("GetSupport: %v", err)
	}
	if !sup.CanTalk() {
		t.Error("CanTalk() = false, want true from the fixture's audioTalk 1")
	}
	if !sup.HasExternStream() {
		t.Error("HasExternStream() = false, want true from the fixture's noExternStream 0")
	}
}

// A camera that does not implement the support read answers 405 rather than
// failing the connection; GetSupport must turn that into a plain-English
// message rather than a raw parse failure on an empty body.
func TestGetSupportReportsAnUnimplementedReadPlainly(t *testing.T) {
	const nonce = "probe-test-nonce-0000000000"
	cam := fakecam.New(t, synthesizeSessionFixture(t, nonce, "", testDeviceInfoXML,
		fakeReply{id: ConfigMessages["support"], body: ""}))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := GetSupport(ctx, conn); err == nil {
		t.Fatal("want an error for an empty support reply")
	}
}

const testAbilityXML = xmlHeader + `<body>
<AbilityInfo version="1.1">
<userName>admin</userName>
<system>
<subModule>
<abilityValue>general_rw, version_ro</abilityValue>
</subModule>
</system>
</AbilityInfo>
</body>`

func TestGetAbilitiesReadsTheAbilityList(t *testing.T) {
	const nonce = "probe-test-nonce-0000000000"
	cam := fakecam.New(t, synthesizeSessionFixture(t, nonce, "", testDeviceInfoXML,
		fakeReply{id: MsgIDAbilityInfo, body: testAbilityXML}))

	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := GetAbilities(ctx, conn)
	if err != nil {
		t.Fatalf("GetAbilities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d abilities, want 2: %+v", len(got), got)
	}
}

// The real proof that GetStreamInfo learns something true: h265_s2c.bin is
// a genuine capture of a camera's main stream, and docs/measurements.md
// records it as 3840x2160 HEVC. TestZZInfoFrameProbe-shaped evidence (an
// ad hoc probe run while building this) confirmed the fixture's own Info
// packet reports exactly that before the first coded frame arrives.
func TestGetStreamInfoLearnsRealResolutionAndCodec(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))

	info, err := GetStreamInfo(context.Background(), cam.Addr(), Options{Password: ""}, StreamMain)
	if err != nil {
		t.Fatalf("GetStreamInfo: %v", err)
	}
	if info.Codec != "H265" {
		t.Errorf("Codec = %q, want H265", info.Codec)
	}
	if info.Width != 3840 || info.Height != 2160 {
		t.Errorf("resolution = %dx%d, want 3840x2160", info.Width, info.Height)
	}
}

// A camera that logs in and then sends no media for a requested stream is
// exactly what "this model does not have that stream" looks like on the
// wire (see GetStreamInfo's own doc comment on why this reads media rather
// than a config message). GetStreamInfo must report that as an error a
// caller can skip past, not hang or panic.
func TestGetStreamInfoReportsNoMediaAsAnError(t *testing.T) {
	fixture := loadFixture(t, "h265_s2c.bin")
	loginOnly := fixturePrefixLen(t, "h265_s2c.bin", 2)
	cam := fakecam.NewPartial(t, fixture, loginOnly)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := GetStreamInfo(ctx, cam.Addr(), Options{Password: ""}, StreamExtern)
	if err == nil {
		t.Fatal("want an error when a stream never sends any media")
	}
}
