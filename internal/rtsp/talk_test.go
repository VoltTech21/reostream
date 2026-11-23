package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
)

// fakeTalker stands in for a camera's speaker and records what reached it.
type fakeTalker struct {
	mu      sync.Mutex
	opens   int
	closes  int
	samples []int16
	rates   map[int]bool
}

func (f *fakeTalker) Open(context.Context) (TalkSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	return fakeSession{f}, nil
}

type fakeSession struct{ f *fakeTalker }

func (s fakeSession) Write(rate int, pcm []int16) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.rates == nil {
		s.f.rates = map[int]bool{}
	}
	s.f.rates[rate] = true
	s.f.samples = append(s.f.samples, pcm...)
	return nil
}
func (s fakeSession) Ping() error { return nil }
func (s fakeSession) Close() error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.closes++
	return nil
}

func (f *fakeTalker) snapshot() (opens, closes, n int, rates map[int]bool, first int16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.samples) > 0 {
		first = f.samples[0]
	}
	return f.opens, f.closes, len(f.samples), f.rates, first
}

func backChannelIn(desc *description.Session) (*description.Media, *format.G711) {
	for _, m := range desc.Medias {
		if !m.IsBackChannel {
			continue
		}
		for _, f := range m.Formats {
			if g, ok := f.(*format.G711); ok {
				return m, g
			}
		}
	}
	return nil, nil
}

func describeWith(t *testing.T, addr, path string, back bool) (*gortsplib.Client, *description.Session) {
	t.Helper()
	u, err := base.ParseURL("rtsp://" + addr + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	tr := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr, RequestBackChannels: back}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	return c, desc
}

// The whole feature, from the client's side: a go2rtc-style client asks for
// the back channel, finds G.711 there, sends speech, and it reaches the
// camera's speaker as PCM. Hanging up releases the camera.
func TestBackChannelAudioReachesTheCamera(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam")
	ft := &fakeTalker{}
	st.SetTalker(ft)
	st.SetSpeaker(true)
	feedUntilReady(t, st, frames)

	c, desc := describeWith(t, srv.Addr(), "cam", true)
	medi, g := backChannelIn(desc)
	if medi == nil {
		t.Fatal("no G.711 back channel offered to a client that asked for one")
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
	// 20 ms packets of a constant level, so what arrives can be checked.
	pcm := make([]byte, 320)
	for i := 0; i < len(pcm); i += 2 {
		pcm[i], pcm[i+1] = 0x10, 0x00 // 4096, big endian
	}
	var payload []byte
	if g.MULaw {
		payload, _ = g711.Mulaw(pcm).Marshal()
	} else {
		payload, _ = g711.Alaw(pcm).Marshal()
	}
	for i := 0; i < 25; i++ {
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
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, n, _, _ := ft.snapshot(); n >= 25*160 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	opens, _, n, rates, first := ft.snapshot()
	if opens != 1 || n < 25*160 {
		t.Fatalf("camera saw %d opens and %d samples, want 1 and %d", opens, n, 25*160)
	}
	if !rates[8000] || len(rates) != 1 {
		t.Errorf("rates %v, want only 8000", rates)
	}
	if first < 3900 || first > 4300 {
		t.Errorf("first sample %d, want about 4096 after G.711", first)
	}

	c.Close()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, closes, _, _, _ := ft.snapshot(); closes == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hanging up did not release the camera's talk session")
}

// A camera with no speaker offers no back channel, so Frigate shows no talk
// button for it, and nothing can ever send it a talk configuration.
func TestNoBackChannelWithoutASpeaker(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam")
	st.SetTalker(&fakeTalker{})
	st.SetSpeaker(false)
	feedUntilReady(t, st, frames)

	if _, desc := describeWith(t, srv.Addr(), "cam", true); func() bool { m, _ := backChannelIn(desc); return m != nil }() {
		t.Fatal("back channel offered for a camera with no speaker")
	}
}

// A plain player, ffmpeg included, never asks for a back channel and must
// not be handed one: an unexpected sendonly track is how a recorder that
// works today breaks after an upgrade.
func TestPlainClientsSeeNoBackChannel(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam")
	st.SetTalker(&fakeTalker{})
	st.SetSpeaker(true)
	feedUntilReady(t, st, frames)

	_, desc := describeWith(t, srv.Addr(), "cam", false)
	for _, m := range desc.Medias {
		if m.IsBackChannel {
			t.Fatal("back channel offered to a client that did not ask for one")
		}
	}
}

// go2rtc repeats the back channel's SETUP when two of its consumers start
// talking at once, and on a refusal it drops the back channel until it
// restarts. A repeated SETUP must get the transport it already has.
func TestRepeatedBackChannelSetupIsAccepted(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam")
	st.SetTalker(&fakeTalker{})
	st.SetSpeaker(true)
	feedUntilReady(t, st, frames)

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	cseq := 0
	send := func(method, url string, hdr ...string) (int, string) {
		cseq++
		msg := fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\n", method, url, cseq)
		for _, h := range hdr {
			msg += h + "\r\n"
		}
		if _, err := conn.Write([]byte(msg + "\r\n")); err != nil {
			t.Fatal(err)
		}
		status, head := 0, ""
		length := 0
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if status == 0 {
				fmt.Sscanf(line, "RTSP/1.0 %d", &status)
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				length, _ = strconv.Atoi(v)
			}
			if strings.HasPrefix(line, "Session: ") {
				head = strings.SplitN(strings.TrimPrefix(line, "Session: "), ";", 2)[0]
			}
		}
		io.CopyN(io.Discard, r, int64(length))
		return status, head
	}

	base := "rtsp://" + srv.Addr() + "/cam"
	if s, _ := send("DESCRIBE", base, "Accept: application/sdp", "Require: www.onvif.org/ver20/backchannel"); s != 200 {
		t.Fatalf("DESCRIBE %d", s)
	}
	s, sess := send("SETUP", base+"/trackID=0", "Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if s != 200 || sess == "" {
		t.Fatalf("SETUP video %d", s)
	}
	back := base + "/trackID=2"
	if s, _ := send("SETUP", back, "Transport: RTP/AVP/TCP;unicast;interleaved=4-5", "Session: "+sess); s != 200 {
		t.Fatalf("SETUP back channel %d", s)
	}
	if s, _ := send("SETUP", back, "Transport: RTP/AVP/TCP;unicast;interleaved=4-5", "Session: "+sess); s != 200 {
		t.Fatalf("repeated SETUP of the back channel answered %d, want 200", s)
	}
}

// 16 bit PCM at 16 kHz, which go2rtc sends as "PCM", reaches the camera at
// 16 kHz rather than being squeezed through G.711's 8.
func TestBackChannelTakes16kPCM(t *testing.T) {
	frames := captureFrames(t)
	srv, st := serverWithStream(t, "cam")
	ft := &fakeTalker{}
	st.SetTalker(ft)
	st.SetSpeaker(true)
	feedUntilReady(t, st, frames)

	c, desc := describeWith(t, srv.Addr(), "cam", true)
	var medi *description.Media
	var l *format.LPCM
	for _, m := range desc.Medias {
		if !m.IsBackChannel {
			continue
		}
		for _, f := range m.Formats {
			if lf, ok := f.(*format.LPCM); ok && lf.SampleRate == 16000 && lf.BitDepth == 16 {
				medi, l = m, lf
			}
		}
	}
	if medi == nil {
		t.Fatal("no 16 kHz L16 back channel offered")
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("play: %v", err)
	}
	enc, err := l.CreateEncoder()
	if err != nil {
		t.Fatal(err)
	}
	// 20 ms at 16 kHz is 320 samples, big endian on the wire.
	pcm := make([]byte, 640)
	for i := 0; i < len(pcm); i += 2 {
		pcm[i], pcm[i+1] = 0x10, 0x00
	}
	for i := 0; i < 25; i++ {
		pkts, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkts {
			p.Timestamp = uint32(i * 320)
			if err := c.WritePacketRTP(medi, p); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, n, _, _ := ft.snapshot(); n >= 25*320 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, n, rates, first := ft.snapshot()
	if n < 25*320 {
		t.Fatalf("camera saw %d samples, want %d", n, 25*320)
	}
	if !rates[16000] || len(rates) != 1 {
		t.Errorf("rates %v, want only 16000", rates)
	}
	if first != 4096 {
		t.Errorf("first sample %d, want 4096 exactly: L16 is lossless", first)
	}
}

// go2rtc (and so Frigate's talk button) uses only the first format of an
// RTSP back channel and cannot send L16 (AlexxIT/go2rtc#696), so G.711 has
// to lead or the mic disappears (#50). L16 stays offered after it for
// clients that pick a format themselves.
func TestBackChannelLeadsWithG711(t *testing.T) {
	m := backChannelMedia()
	g, ok := m.Formats[0].(*format.G711)
	if !ok || !g.MULaw || g.PayloadTyp != 0 {
		t.Fatalf("first back channel format is %T %+v, want G.711 mu-law PT 0", m.Formats[0], m.Formats[0])
	}
	if g, ok := m.Formats[1].(*format.G711); !ok || g.MULaw || g.PayloadTyp != 8 {
		t.Fatalf("second back channel format is %T, want G.711 A-law PT 8", m.Formats[1])
	}
	var l16 bool
	for _, f := range m.Formats {
		if _, ok := f.(*format.LPCM); ok {
			l16 = true
		}
	}
	if !l16 {
		t.Fatal("16 kHz L16 is no longer offered")
	}
}
