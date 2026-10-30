package rtsp

import (
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/auth"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
)

// loginServer is a ready stream behind a login, offering only the given
// authentication methods, or gortsplib's default of both when none. prepare,
// if not nil, runs before the stream is ready, which is when its
// description is fixed.
func loginServer(t *testing.T, prepare func(*Stream), methods ...auth.VerifyMethod) (*Server, *Stream) {
	t.Helper()
	frames := captureFrames(t)
	srv := New("127.0.0.1:8554")
	srv.RequireLogin("frigate", "s3cret")
	srv.srv.AuthMethods = methods
	st := srv.Add("cam")
	if prepare != nil {
		prepare(st)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Close)
	feedUntilReady(t, st, frames)
	return srv, st
}

// describeAs runs DESCRIBE with the login in userinfo ("" for none).
func describeAs(t *testing.T, addr, userinfo string) error {
	t.Helper()
	if userinfo != "" {
		userinfo += "@"
	}
	u, err := base.ParseURL("rtsp://" + userinfo + addr + "/cam")
	if err != nil {
		t.Fatal(err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.Describe(u)
	return err
}

// With a login set, a reader without one, or with the wrong one, gets no
// stream, and one with the right one does, over Basic and Digest both:
// ffmpeg, Frigate and go2rtc answer whichever the server offers first.
func TestALoginIsRequiredWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []auth.VerifyMethod
	}{
		{"default", nil},
		{"basic", []auth.VerifyMethod{auth.VerifyMethodBasic}},
		{"digest", []auth.VerifyMethod{auth.VerifyMethodDigestMD5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := loginServer(t, nil, tc.methods...)
			err := describeAs(t, srv.Addr(), "")
			if err == nil || !strings.Contains(err.Error(), "401") {
				t.Errorf("no login: err = %v, want a 401", err)
			}
			if err := describeAs(t, srv.Addr(), "frigate:wrong"); err == nil {
				t.Error("the wrong password was let in")
			}
			if err := describeAs(t, srv.Addr(), "someone:s3cret"); err == nil {
				t.Error("the wrong username was let in")
			}
			if err := describeAs(t, srv.Addr(), "frigate:s3cret"); err != nil {
				t.Errorf("the right login was refused: %v", err)
			}
		})
	}
}

// A path that does not exist answers 401 too, not 404, so names cannot be
// probed without the login.
func TestAnUnknownPathDoesNotLeakPastTheLogin(t *testing.T) {
	srv, _ := loginServer(t, nil)
	u, err := base.ParseURL("rtsp://" + srv.Addr() + "/nosuchcamera")
	if err != nil {
		t.Fatal(err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Describe(u); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401", err)
	}
}

// SETUP is checked on its own, so a client that skips DESCRIBE, having the
// description from somewhere else, still cannot attach to a stream.
func TestSetupWithoutALoginIsRefused(t *testing.T) {
	srv, st := loginServer(t, nil)
	u, err := base.ParseURL("rtsp://" + srv.Addr() + "/cam")
	if err != nil {
		t.Fatal(err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	desc := st.description()
	if _, err := c.Setup(u, desc.Medias[0], 0, 0); err == nil {
		t.Fatal("SETUP without a login was accepted")
	}
}

// Talk works for a reader that logged in: go2rtc (and Frigate through it)
// sets up the back channel on the same authenticated connection.
func TestBackChannelWorksBehindALogin(t *testing.T) {
	ft := &fakeTalker{}
	srv, _ := loginServer(t, func(st *Stream) {
		st.SetTalker(ft)
		st.SetSpeaker(true)
	})

	u, err := base.ParseURL("rtsp://frigate:s3cret@" + srv.Addr() + "/cam")
	if err != nil {
		t.Fatal(err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr, RequestBackChannels: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	medi, g := backChannelIn(desc)
	if medi == nil {
		t.Fatal("no back channel offered behind the login")
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("play: %v", err)
	}

	enc, err := g.CreateEncoder()
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 320)
	var payload []byte
	if g.MULaw {
		payload, _ = g711.Mulaw(pcm).Marshal()
	} else {
		payload, _ = g711.Alaw(pcm).Marshal()
	}
	for i := 0; i < 10; i++ {
		pkts, err := enc.Encode(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkts {
			p.Timestamp = uint32(i * 160)
			if err := c.WritePacketRTP(medi, p); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if opens, _, n, _, _ := ft.snapshot(); opens == 1 && n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("talk from a logged-in reader never reached the camera")
}
