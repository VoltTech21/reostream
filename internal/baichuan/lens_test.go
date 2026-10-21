package baichuan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// The three candidate ways of asking for a second lens put exactly what
// they say on the wire: the header's stream number and, or, the Preview
// document's logicChannel. Nobody can check the camera's side here, so this
// is what makes a TrackMix owner's report mean something.
func TestSecondLensRequests(t *testing.T) {
	for _, tc := range []struct {
		method    int
		stream    string
		wantByte  byte
		wantLogic bool
	}{
		{0, StreamMain, 2, false},
		{1, StreamSub, 3, false},
		{2, StreamMain, 2, true},
		{3, StreamSub, 1, true},
	} {
		const nonce = "lens-test-nonce-000000000000"
		cam := fakecam.New(t, synthesizeSessionFixture(t, nonce, "", testDeviceInfoXML))
		conn, err := Dial(context.Background(), cam.Addr(), Options{Lens: 1, LensMethod: tc.method})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.StartVideo(tc.stream); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		deadline := time.Now().Add(2 * time.Second)
		for cam.Closes() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		key := AESKey(nonce, "")
		found := false
		walkMessages(cam.Received(), func(h Header, body []byte) bool {
			if h.MsgID != MsgIDVideo || found {
				return true
			}
			found = true
			if got := byte(h.EncOffset >> 8); got != tc.wantByte {
				t.Errorf("method %d %s: header stream %d, want %d", tc.method, tc.stream, got, tc.wantByte)
			}
			x, err := AESDecrypt(key, body)
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(string(x), "<logicChannel>1</logicChannel>"); has != tc.wantLogic {
				t.Errorf("method %d %s: logicChannel sent = %v, want %v\n%s", tc.method, tc.stream, has, tc.wantLogic, x)
			}
			return true
		})
		if !found {
			t.Fatalf("method %d: no video request sent", tc.method)
		}
	}
}

// Without lens set, nothing about a request changes.
func TestFirstLensRequestIsUnchanged(t *testing.T) {
	c := &Conn{}
	if c.streamByte(StreamMain) != 0 || c.streamByte(StreamSub) != 1 || c.streamByte(StreamExtern) != 4 || c.logicChannel() != nil {
		t.Fatal("a camera with no lens setting asks for something different")
	}
}
