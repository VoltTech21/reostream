package camctl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// waitForCloses polls cam.Closes() until it reaches want or waitTimeout
// elapses, then fails with the same message either sampling once would have
// given. This exists because Close on the client side only puts the FIN on
// the wire; fakecam's own accept-loop goroutine records a close after *it*
// observes the peer go away, which happens on a separate goroutine, so
// reading Closes() the instant probeCamera returns races that goroutine
// rather than being ordered after it.
const waitCloseTimeout = 2 * time.Second

func waitForCloses(t *testing.T, cam *fakecam.Camera, want int) {
	t.Helper()
	deadline := time.Now().Add(waitCloseTimeout)
	for {
		if got := cam.Closes(); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("fullCam recorded %d closes, want %d: the live connection at return time must be closed exactly once", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProbeSortsRepliesIntoSupportedWantsParamsAndAbsent(t *testing.T) {
	// A message a camera does not implement answers 405 rather than failing
	// the connection, which is what makes sending every known read safe and
	// is the only honest way to find out what a model has.
	cases := []struct {
		status int16
		xml    string
		want   string
	}{
		{200, "<body/>", "supported"},
		{400, "", "wants params"},
		{405, "", "absent"},
	}
	for _, tc := range cases {
		got := classify(tc.status, []byte(tc.xml))
		if got != tc.want {
			t.Fatalf("status %d classified %q, want %q", tc.status, got, tc.want)
		}
	}
}

const testXMLHeader = `<?xml version="1.0" encoding="UTF-8"?>`
const testProbeNonce = "camctl-probe-test-nonce-000000"
const testProbeDeviceInfo = testXMLHeader + `<body><DeviceInfo><typeInfo>IPC</typeInfo></DeviceInfo></body>`

// buildMessage frames body under h, setting MsgLen from its length the same
// way baichuan's own Writer does. It lives here, not in package baichuan,
// because a fake camera's replies are exactly the kind of bytes no
// committed capture contains: this program asks a camera for messages a
// real capture was never taken against.
func buildMessage(h baichuan.Header, body []byte) []byte {
	h.MsgLen = uint32(len(body))
	return append(h.Encode(), body...)
}

// loginHandshake builds the two messages a Conn reads during login: the
// nonce reply, then a login reply carrying deviceInfoXML. Both are
// BC-encrypted, the cipher a Conn has before the AES key derived from the
// nonce exists.
func loginHandshake(nonce, deviceInfoXML string) []byte {
	plainNonce := testXMLHeader + `<body><Encryption version="1.1"><type>md5</type><nonce>` + nonce + `</nonce></Encryption></body>`
	nonceReply := buildMessage(baichuan.Header{
		MsgID: baichuan.MsgIDLogin, Class: baichuan.ClassModern20,
		EncByte: baichuan.NegotiateByte, DirByte: baichuan.DirReply,
	}, baichuan.BCCrypt(0, []byte(plainNonce)))
	loginReply := buildMessage(baichuan.Header{MsgID: baichuan.MsgIDLogin, Class: baichuan.ClassZero},
		baichuan.BCCrypt(0, []byte(deviceInfoXML)))
	return append(nonceReply, loginReply...)
}

// statusReply builds one AES-encrypted post-login reply carrying status,
// the same way every message past login is encrypted (see login_s2c.bin).
func statusReply(t *testing.T, key []byte, id uint32, status int16, body string) []byte {
	t.Helper()
	enc, err := baichuan.AESEncrypt(key, []byte(body))
	if err != nil {
		t.Fatalf("encrypt reply %d: %v", id, err)
	}
	return buildMessage(baichuan.Header{
		MsgID:   id,
		Class:   baichuan.ClassZero,
		EncByte: byte(uint16(status) & 0xff),
		DirByte: byte(uint16(status) >> 8),
	}, enc)
}

// prefixLen returns the byte length of the first n messages of fixture, so
// a hang-up can be simulated by truncating a fixture at a message boundary
// rather than an arbitrary byte offset that might split a header.
func prefixLen(t *testing.T, fixture []byte, n int) int {
	t.Helper()
	off := 0
	for i := 0; i < n; i++ {
		h, hn, err := baichuan.DecodeHeader(fixture[off:])
		if err != nil {
			t.Fatalf("decode message %d: %v", i, err)
		}
		off += hn + int(h.MsgLen)
	}
	return off
}

// buildProbeFixture answers every name baichuan.ConfigNames knows: the
// first with 200, the second with 400, and everything else with 405. It
// exists so a probeCamera test can assert against a known, complete answer
// for the entire real sweep rather than an arbitrary subset.
func buildProbeFixture(t *testing.T) (fixture []byte, names []string) {
	t.Helper()
	names = baichuan.ConfigNames()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture = loginHandshake(testProbeNonce, testProbeDeviceInfo)
	for i, name := range names {
		id := baichuan.ConfigMessages[name]
		switch i {
		case 0:
			fixture = append(fixture, statusReply(t, key, id, 200, testXMLHeader+"<body/>")...)
		case 1:
			fixture = append(fixture, statusReply(t, key, id, 400, "")...)
		default:
			fixture = append(fixture, statusReply(t, key, id, 405, "")...)
		}
	}
	return fixture, names
}

// A camera that hangs up on a request has still told us something worth
// keeping, and cmd/reocam's own sweep already carries on past it by
// reconnecting. probeCamera must do the same: record the message it died
// on and pick the sweep back up on a fresh connection.
func TestProbeCameraReconnectsAfterAHangupAndRecordsIt(t *testing.T) {
	fixture, names := buildProbeFixture(t)
	if len(names) < 10 {
		t.Fatalf("only %d config names, test needs more to pick a drop point", len(names))
	}
	const dropIndex = 5 // arbitrary, well clear of the 200/400 cases at 0 and 1

	cut := prefixLen(t, fixture, 2+dropIndex) // 2 login messages, then the first dropIndex replies
	dropCam := fakecam.NewDropAfter(t, fixture, cut)
	fullCam := fakecam.New(t, fixture)

	calls := 0
	dial := func(ctx context.Context, cam Camera) (*baichuan.Conn, error) {
		calls++
		addr := fullCam.Addr()
		if calls == 1 {
			addr = dropCam.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}

	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	cam, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}

	probes, err := s.probeCamera(context.Background(), cam)
	if err != nil {
		t.Fatalf("probeCamera: %v", err)
	}
	if len(probes) != len(names) {
		t.Fatalf("got %d probes, want %d (one per config name)", len(probes), len(names))
	}
	if calls != 2 {
		t.Fatalf("dial was called %d times, want 2 (the original connection, then one reconnect)", calls)
	}

	// The second dial goes to fullCam and is the connection probeCamera is
	// still holding when it returns, so it must be the one the deferred
	// close reaches. dropCam's own fakecam variant does not track this
	// (NewDropAfter simulates the *camera* dropping the connection, not the
	// client closing it, so there is nothing on that side to count), but
	// fullCam's is exactly the signal a leaked-connection bug shows up as:
	// `defer conn.Close()` bound to the stale, already-hung-up connection
	// would never touch this one at all, and it would stay at 0 forever.
	// Poll rather than sample once: probeCamera returning only means the
	// close has gone out on the wire, not that fullCam's own accept-loop
	// goroutine has yet observed the peer go away and incremented its
	// counter, so reading Closes() immediately races that goroutine.
	waitForCloses(t, fullCam, 1)

	for i, p := range probes {
		if p.Name != names[i] {
			t.Fatalf("probe %d is %q, want %q", i, p.Name, names[i])
		}
		switch i {
		case dropIndex:
			if !p.HungUp {
				t.Errorf("probe %q: HungUp = false, want true (this is the message the connection died on)", p.Name)
			}
		case 0:
			if !p.Supported || p.Status != 200 {
				t.Errorf("probe %q: Supported=%v Status=%d, want Supported=true Status=200", p.Name, p.Supported, p.Status)
			}
		case 1:
			if !p.WantsParams || p.Status != 400 {
				t.Errorf("probe %q: WantsParams=%v Status=%d, want WantsParams=true Status=400", p.Name, p.WantsParams, p.Status)
			}
		default:
			if !p.Absent || p.Status != 405 {
				t.Errorf("probe %q: Absent=%v Status=%d, want Absent=true Status=405", p.Name, p.Absent, p.Status)
			}
		}
	}
}

const testAbilityXML = testXMLHeader + `<body>
<AbilityInfo version="1.1">
<userName>admin</userName>
<system>
<subModule>
<abilityValue>general_rw</abilityValue>
</subModule>
</system>
</AbilityInfo>
</body>`

const testSupportXML = testXMLHeader + `<body>
<Support version="1.1">
<channelNum>1</channelNum>
<audioTalk>1</audioTalk>
<noExternStream>0</noExternStream>
</Support>
</body>`

// The camera page dials once for GetSupport and GetAbilities and once more
// inside probeCamera. Both connections replay the same fixture, so this
// pins that the page renders a real model summary, a real ability list, and
// the probe's own per-class counts, in one request.
func TestServeCameraRendersSupportAbilitiesAndProbeCounts(t *testing.T) {
	fixture, names := buildProbeFixture(t)
	key := baichuan.AESKey(testProbeNonce, "")
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDAbilityInfo, 200, testAbilityXML)...)

	// The support reply the generic sweep above would have sent for
	// "support" is a bare 405; overwrite the fixture's answer for that one
	// name with real Support XML so GetSupport has something to parse.
	supportID := baichuan.ConfigMessages["support"]
	fixture = replaceReply(t, fixture, supportID, key, testSupportXML)

	cam := fakecam.New(t, fixture)
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}

	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/camera/cam1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	if !strings.Contains(html, "general") {
		t.Error("page does not show the ability list from GetAbilities")
	}
	if !strings.Contains(html, "yes") {
		t.Error("page does not show a yes/no field from GetSupport")
	}
	// Everything but the 200 and 400 cases from buildProbeFixture, and
	// "support" itself, which replaceReply moves from absent to supported.
	wantAbsent := len(names) - 3
	if !strings.Contains(html, strconv.Itoa(wantAbsent)) {
		t.Errorf("page does not show the absent count %d", wantAbsent)
	}
}

// replaceReply swaps out the encrypted body of the first reply for id in
// fixture, keeping every other message (and the surrounding message
// framing) untouched. It exists so buildProbeFixture's blanket 405 answer
// for one particular message can be overridden with real content, without
// hand-building the whole fixture a second time.
func replaceReply(t *testing.T, fixture []byte, id uint32, key []byte, newBody string) []byte {
	t.Helper()
	enc, err := baichuan.AESEncrypt(key, []byte(newBody))
	if err != nil {
		t.Fatalf("encrypt replacement body: %v", err)
	}
	off := 0
	for off < len(fixture) {
		h, hn, err := baichuan.DecodeHeader(fixture[off:])
		if err != nil {
			t.Fatalf("decode fixture at %d: %v", off, err)
		}
		end := off + hn + int(h.MsgLen)
		if h.MsgID == id {
			h.EncByte = byte(200)
			h.DirByte = 0
			replaced := buildMessage(h, enc)
			out := append([]byte{}, fixture[:off]...)
			out = append(out, replaced...)
			out = append(out, fixture[end:]...)
			return out
		}
		off = end
	}
	t.Fatalf("fixture has no reply for message %d", id)
	return nil
}
