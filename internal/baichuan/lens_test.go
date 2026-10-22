package baichuan

import (
	"context"
	"fmt"
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
		wantChan  byte // header channel
		wantDoc   int  // Preview channelId
	}{
		{0, StreamMain, 2, false, 0, 0},
		{1, StreamSub, 3, false, 0, 0},
		{2, StreamMain, 2, true, 0, 0},
		{3, StreamSub, 1, true, 0, 0},
		{4, StreamMain, 0, false, 1, 1},
		{5, StreamSub, 1, false, 0, 1},
		{6, StreamMain, 4, false, 0, 0},
		{6, StreamSub, 2, false, 0, 0},
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
			if got := byte(h.EncOffset); got != tc.wantChan {
				t.Errorf("method %d %s: header channel %d, want %d", tc.method, tc.stream, got, tc.wantChan)
			}
			x, err := AESDecrypt(key, body)
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(string(x), "<logicChannel>1</logicChannel>"); has != tc.wantLogic {
				t.Errorf("method %d %s: logicChannel sent = %v, want %v\n%s", tc.method, tc.stream, has, tc.wantLogic, x)
			}
			if tc.method == 6 {
				want := map[string]string{StreamMain: StreamExtern, StreamSub: StreamMobile}[tc.stream]
				if !strings.Contains(string(x), "<streamType>"+want+"</streamType>") {
					t.Errorf("method 6 %s: Preview does not ask for %s\n%s", tc.stream, want, x)
				}
			}
			if want := fmt.Sprintf("<channelId>%d</channelId>", tc.wantDoc); !strings.Contains(string(x), want) {
				t.Errorf("method %d %s: Preview lacks %s\n%s", tc.method, tc.stream, want, x)
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
