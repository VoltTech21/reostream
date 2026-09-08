package ts

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// encryptionReply mirrors the tiny slice of the camera's negotiation XML this
// test needs. baichuan's own parseNonce is unexported and internal/baichuan is
// not to be modified for this plan, so the handful of fields are duplicated
// here rather than exporting anything from that package.
type encryptionReply struct {
	XMLName xml.Name `xml:"body"`
	Enc     struct {
		Nonce string `xml:"nonce"`
	} `xml:"Encryption"`
}

func parseNonce(t *testing.T, x []byte) string {
	t.Helper()
	var b encryptionReply
	if err := xml.Unmarshal(x, &b); err != nil {
		t.Fatalf("parse nonce: %v", err)
	}
	if b.Enc.Nonce == "" {
		t.Fatal("no nonce in the negotiation reply")
	}
	return b.Enc.Nonce
}

func frame(kind baichuan.FrameKind, micros uint32, payload ...byte) baichuan.Frame {
	// A frame whose Data already begins with a NAL start code, so Video()
	// returns it unchanged.
	d := append([]byte{0, 0, 0, 1}, payload...)
	return baichuan.Frame{Kind: kind, Codec: "h265", Micros: micros, Data: d}
}

func TestMuxerRejectsUnknownCodec(t *testing.T) {
	if _, err := NewMuxer("vp9"); err == nil {
		t.Fatal("expected an error for an unsupported codec")
	}
}

func TestMuxerEmitsWholePackets(t *testing.T) {
	m, err := NewMuxer("h265")
	if err != nil {
		t.Fatal(err)
	}
	out := m.Frame(frame(baichuan.FrameIFrame, 1000, make([]byte, 5000)...))
	if len(out) == 0 {
		t.Fatal("muxer produced nothing for an I frame")
	}
	if len(out)%PacketSize != 0 {
		t.Fatalf("output is %d bytes, not a multiple of %d", len(out), PacketSize)
	}
	for i := 0; i < len(out); i += PacketSize {
		if out[i] != 0x47 {
			t.Fatalf("packet at offset %d does not start with sync", i)
		}
	}
}

func TestMuxerWaitsForAKeyframe(t *testing.T) {
	// Starting a decoder mid GOP only makes it complain about references it
	// never saw, so nothing may be emitted before the first I frame.
	m, _ := NewMuxer("h264")
	if got := m.Frame(frame(baichuan.FramePFrame, 1000, 1, 2, 3)); len(got) != 0 {
		t.Fatalf("emitted %d bytes before the first keyframe", len(got))
	}
	if got := m.Frame(frame(baichuan.FrameIFrame, 2000, 1, 2, 3)); len(got) == 0 {
		t.Fatal("emitted nothing for the first keyframe")
	}
	if got := m.Frame(frame(baichuan.FramePFrame, 3000, 1, 2, 3)); len(got) == 0 {
		t.Fatal("emitted nothing for a P frame after the keyframe")
	}
}

func TestMuxerIgnoresNonVideoFrames(t *testing.T) {
	m, _ := NewMuxer("h264")
	m.Frame(frame(baichuan.FrameIFrame, 1000, 1))
	for _, k := range []baichuan.FrameKind{baichuan.FrameInfo, baichuan.FrameAAC, baichuan.FrameADPCM} {
		if got := m.Frame(frame(k, 2000, 1, 2)); len(got) != 0 {
			t.Errorf("kind %v produced %d bytes, want 0 in a video only muxer", k, len(got))
		}
	}
}

func TestMuxerRepeatsTablesSoLateJoinersCanDecode(t *testing.T) {
	// A client joining mid stream needs the PAT and PMT again, so they are
	// resent periodically rather than only at the start.
	m, _ := NewMuxer("h264")
	var pmtPackets int
	for i := 0; i < 200; i++ {
		out := m.Frame(frame(baichuan.FrameIFrame, uint32(i)*40000, 1, 2, 3))
		for j := 0; j < len(out); j += PacketSize {
			pid := PID(out[j+1]&0x1F)<<8 | PID(out[j+2])
			if pid == PIDPMT {
				pmtPackets++
			}
		}
	}
	if pmtPackets < 2 {
		t.Fatalf("PMT appeared %d times in 200 frames, want it repeated", pmtPackets)
	}
}

func TestMuxerHeaderIsSelfContained(t *testing.T) {
	m, _ := NewMuxer("h265")
	h := m.Header()
	if len(h) != 2*PacketSize {
		t.Fatalf("header is %d bytes, want one PAT and one PMT packet", len(h))
	}
	var sawPAT, sawPMT bool
	for i := 0; i < len(h); i += PacketSize {
		switch PID(h[i+1]&0x1F)<<8 | PID(h[i+2]) {
		case PIDPAT:
			sawPAT = true
		case PIDPMT:
			sawPMT = true
		}
	}
	if !sawPAT || !sawPMT {
		t.Fatalf("header missing PAT (%v) or PMT (%v)", sawPAT, sawPMT)
	}
}

func TestMuxerPTSAdvancesWithCameraTime(t *testing.T) {
	m, _ := NewMuxer("h264")
	first := m.Frame(frame(baichuan.FrameIFrame, 1000000, 1, 2, 3))
	second := m.Frame(frame(baichuan.FrameIFrame, 1040000, 1, 2, 3))
	p1, ok1 := findPTS(first)
	p2, ok2 := findPTS(second)
	if !ok1 || !ok2 {
		t.Fatal("could not find PTS in the emitted packets")
	}
	if got := p2 - p1; got != 3600 {
		t.Fatalf("PTS advanced by %d for a 40ms gap, want 3600", got)
	}
}

// findPTS locates the first PES header in a run of transport packets and
// decodes its 33 bit PTS.
func findPTS(b []byte) (uint64, bool) {
	for i := 0; i+PacketSize <= len(b); i += PacketSize {
		p := b[i : i+PacketSize]
		if p[1]&0x40 == 0 {
			continue // not a payload unit start
		}
		pid := PID(p[1]&0x1F)<<8 | PID(p[2])
		if pid != PIDVideo {
			continue
		}
		pes := p[4:]
		if len(pes) < 14 || pes[0] != 0x00 || pes[1] != 0x00 || pes[2] != 0x01 {
			continue
		}
		f := pes[9:14]
		v := uint64(f[0]>>1&0x07)<<30 |
			uint64(f[1])<<22 | uint64(f[2]>>1)<<15 |
			uint64(f[3])<<7 | uint64(f[4]>>1)
		return v, true
	}
	return 0, false
}

func TestMuxRealCaptureProducesADecodableStream(t *testing.T) {
	// internal/baichuan/testdata/h265_s2c.bin is a real camera capture. If
	// the muxer can turn it into a stream ffprobe accepts, the format is
	// right in a way no synthetic test can show.
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	b, err := os.ReadFile("../baichuan/testdata/h265_s2c.bin")
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	// Nonce lives in the first message, plaintext. A separate reader keeps
	// this lookup from disturbing the main walk's message count below.
	nonceReader := baichuan.NewReader(bytes.NewReader(b))
	first, err := nonceReader.Next()
	if err != nil {
		t.Fatalf("read first message: %v", err)
	}
	key := baichuan.AESKey(parseNonce(t, first.XML), "")

	r := baichuan.NewReader(bytes.NewReader(b))
	d := baichuan.NewDepacketiser()
	m, err := NewMuxer("h265")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	out.Write(m.Header())

	var fedFrames int
	for i := 0; ; i++ {
		msg, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		// Message 0 is the negotiation reply, message 1 the login reply;
		// both are plaintext. Everything from message 2 on is encrypted
		// media, so the key is set once message 1 has been consumed.
		if i == 1 {
			r.SetAESKey(key)
		}
		if len(msg.Payload) == 0 {
			continue
		}
		d.Write(msg.Payload, msg.StartsPacket)
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
				continue
			}
			out.Write(m.Frame(f))
			fedFrames++
		}
	}
	if fedFrames == 0 {
		t.Fatal("no video frames decoded from the capture")
	}

	tmp, err := os.CreateTemp(t.TempDir(), "reostream-*.ts")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := tmp.Write(out.Bytes()); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-count_frames",
		"-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames,codec_name",
		"-of", "csv=p=0",
		tmp.Name(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffprobe failed: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("ffprobe wrote to stderr: %s", stderr.String())
	}

	var line string
	for _, l := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
	}
	fields := strings.Split(line, ",")
	if len(fields) != 2 {
		t.Fatalf("unexpected ffprobe output: %q", stdout.String())
	}
	codecName, nbReadFrames := fields[0], fields[1]
	if codecName != "hevc" {
		t.Errorf("codec_name = %q, want hevc", codecName)
	}
	got, err := strconv.Atoi(nbReadFrames)
	if err != nil {
		t.Fatalf("parse nb_read_frames %q: %v", nbReadFrames, err)
	}
	if got != fedFrames {
		t.Errorf("ffprobe read %d frames, muxer was fed %d", got, fedFrames)
	}
	t.Logf("ffprobe: codec_name=%s nb_read_frames=%s (fed %d frames)", codecName, nbReadFrames, fedFrames)
}
