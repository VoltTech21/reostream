package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

const testSnapNonce = "camctl-snap-test-nonce-0000000"

// testJPEG is enough of a JPEG to pass the start-of-image check, plus a
// body long enough to need more than one message.
var testJPEG = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x5a}, 300)...)

// snapFixture is a camera answering one snap the way the eight wired
// cameras here do: one XML reply naming the file and its size, then the
// bytes across further messages, the last padded past that size.
func snapFixture(t *testing.T, img []byte) []byte {
	t.Helper()
	key := baichuan.AESKey(testSnapNonce, "")
	fixture := loginHandshake(testSnapNonce, testProbeDeviceInfo)
	reply := fmt.Sprintf(testXMLHeader+`<body><Snap version="1.1"><channelId>0</channelId><fileName>01_20261103120000000.jpg</fileName><time>0</time><pictureSize>%d</pictureSize></Snap></body>`, len(img))
	fixture = append(fixture, statusReply(t, key, baichuan.MsgIDSnap, 200, reply)...)

	ext, err := baichuan.AESEncrypt(key, []byte(testXMLHeader+`<Extension version="1.1"><binaryData>1</binaryData></Extension>`))
	if err != nil {
		t.Fatal(err)
	}
	padded := append(append([]byte{}, img...), make([]byte, 7)...)
	half := len(padded) / 2
	for _, part := range [][]byte{padded[:half], padded[half:]} {
		h := baichuan.Header{MsgID: baichuan.MsgIDSnap, Class: baichuan.ClassModern24, PayloadOff: uint32(len(ext))}
		fixture = append(fixture, buildMessage(h, append(append([]byte{}, ext...), part...))...)
	}
	return fixture
}

func snapServer(t *testing.T, dial func(ctx context.Context, cam Camera) (*baichuan.Conn, error)) *httptest.Server {
	t.Helper()
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func dialFake(cam *fakecam.Camera) func(ctx context.Context, _ Camera) (*baichuan.Conn, error) {
	return func(ctx context.Context, _ Camera) (*baichuan.Conn, error) {
		return baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Password: ""})
	}
}

func getSnap(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// The picture is the camera's own bytes, trimmed to the size it declared,
// labelled as a JPEG and never cached; the connection it came over is
// closed afterwards, which on a battery camera is what lets it sleep.
func TestSnapshotServesTheCamerasOwnJPEG(t *testing.T) {
	cam := fakecam.New(t, snapFixture(t, testJPEG))
	ts := snapServer(t, dialFake(cam))

	resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg?stream=sub")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, testJPEG) {
		t.Fatalf("got %d bytes, want the %d the camera declared", len(body), len(testJPEG))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store: every request is a new picture", cc)
	}
	waitForCloses(t, cam, 1)

	key := baichuan.AESKey(testSnapNonce, "")
	sent := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDSnap, key)
	if !strings.Contains(sent, "<streamType>sub</streamType>") {
		t.Errorf("the snap asked for the wrong stream:\n%s", sent)
	}
}

// No stream named means main, the full resolution, which is what someone
// asking for "a picture" means.
func TestSnapshotDefaultsToMain(t *testing.T) {
	cam := fakecam.New(t, snapFixture(t, testJPEG))
	ts := snapServer(t, dialFake(cam))

	if resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	waitForCloses(t, cam, 1)
	sent := decodedSetConfigBody(t, cam.Received(), baichuan.MsgIDSnap, baichuan.AESKey(testSnapNonce, ""))
	if !strings.Contains(sent, "<streamType>main</streamType>") {
		t.Errorf("the snap asked for the wrong stream:\n%s", sent)
	}
}

func TestSnapshotRefusesAnUnknownStreamWithoutDialling(t *testing.T) {
	dialled := false
	ts := snapServer(t, func(ctx context.Context, _ Camera) (*baichuan.Conn, error) {
		dialled = true
		return nil, errors.New("must not dial")
	})
	resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg?stream=extern")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if dialled {
		t.Error("dialled the camera for a stream the snap cannot ask for")
	}
}

func TestSnapshotOfAnUnknownCameraIs404(t *testing.T) {
	ts := snapServer(t, nil)
	if resp, body := getSnap(t, ts.URL+"/cameras/nope/snapshot.jpg"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// A model that does not make stills answers 405; that is the camera's
// answer, reported as such, not a timeout and not an empty image.
func TestSnapshotReportsACameraThatRefuses(t *testing.T) {
	key := baichuan.AESKey(testSnapNonce, "")
	fixture := append(loginHandshake(testSnapNonce, testProbeDeviceInfo),
		statusReply(t, key, baichuan.MsgIDSnap, int16(baichuan.StatusNotImplemented), "")...)
	ts := snapServer(t, dialFake(fakecam.New(t, fixture)))

	resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "405") {
		t.Errorf("the refusal does not say what the camera answered: %s", body)
	}
}

// Bytes that are not a JPEG are not labelled as one.
func TestSnapshotRefusesBytesThatAreNotAJPEG(t *testing.T) {
	cam := fakecam.New(t, snapFixture(t, bytes.Repeat([]byte{0x00}, 64)))
	ts := snapServer(t, dialFake(cam))
	resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct == "image/jpeg" {
		t.Error("labelled non-JPEG bytes image/jpeg")
	}
}

// A camera that logs in and then sends nothing is a timeout, bounded by
// snapWait, and the connection is still closed.
func TestSnapshotTimesOutOnASilentCamera(t *testing.T) {
	old := snapWait
	snapWait = 300 * time.Millisecond
	t.Cleanup(func() { snapWait = old })

	login := loginHandshake(testSnapNonce, testProbeDeviceInfo)
	cam := fakecam.NewPartial(t, login, len(login))
	ts := snapServer(t, dialFake(cam))

	start := time.Now()
	resp, body := getSnap(t, ts.URL+"/cameras/cam1/snapshot.jpg")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v to give up; snapWait is %v", took, snapWait)
	}
}

// A camera that cannot be reached is the network's answer (502); one that
// does not answer in time is a timeout (504), worded for a battery camera
// as not waking.
func TestSnapshotDialFailures(t *testing.T) {
	refused := snapServer(t, func(ctx context.Context, _ Camera) (*baichuan.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	if resp, body := getSnap(t, refused.URL+"/cameras/cam1/snapshot.jpg"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("refused dial: status %d: %s", resp.StatusCode, body)
	}

	code, msg := snapDialError(Camera{Name: "argus", UID: "UID1"}, context.DeadlineExceeded)
	if code != http.StatusGatewayTimeout || !strings.Contains(msg, "did not wake") {
		t.Errorf("battery timeout: %d %q", code, msg)
	}
	code, msg = snapDialError(Camera{Name: "gate"}, fmt.Errorf("baichuan: dial: %w", context.DeadlineExceeded))
	if code != http.StatusGatewayTimeout || strings.Contains(msg, "wake") {
		t.Errorf("wired timeout: %d %q", code, msg)
	}
	code, _ = snapDialError(Camera{Name: "gate"}, errCameraSilent)
	if code != http.StatusServiceUnavailable {
		t.Errorf("a camera that timed out moments ago: %d, want 503", code)
	}
}
