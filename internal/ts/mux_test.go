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

func TestNewMuxerAcceptsCodecRegardlessOfCase(t *testing.T) {
	// baichuan.Frame.Codec comes off the wire as "H264"/"H265"; docs, tests
	// and a config file all write it lowercase. NewMuxer is the one place
	// every caller passes through, so it must accept both.
	for _, codec := range []string{"H265", "h265", "H264"} {
		if _, err := NewMuxer(codec); err != nil {
			t.Errorf("NewMuxer(%q): %v", codec, err)
		}
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

func TestPTSAndPCRWrapAt2Pow33Explicitly(t *testing.T) {
	// PTS and PCR are both 33 bit fields. A value past 2^33 ticks (about
	// 26.5 hours of streaming) must wrap to the same bytes as the equivalent
	// small value, which is what a player expects: without the mask this
	// still happens today, by accident, because the bit twiddling below only
	// ever reads the bits that survive it, but that is not something a future
	// change to either function should be able to quietly break.
	const past2Pow33 = uint64(1)<<33 + 12345

	ptsField := make([]byte, 5)
	writePTS(ptsField, past2Pow33)
	wantField := make([]byte, 5)
	writePTS(wantField, 12345)
	if !bytes.Equal(ptsField, wantField) {
		t.Fatalf("PTS past 2^33 encoded as % x, want the same bytes as the wrapped value % x", ptsField, wantField)
	}

	afPast := make([]byte, PacketSize-4)
	writeAdaptation(afPast, PacketSize-4, true, past2Pow33)
	afWrapped := make([]byte, PacketSize-4)
	writeAdaptation(afWrapped, PacketSize-4, true, 12345)
	if !bytes.Equal(afPast[:12], afWrapped[:12]) {
		t.Fatalf("PCR past 2^33 encoded as % x, want the same bytes as the wrapped value % x", afPast[:12], afWrapped[:12])
	}
}

func TestMuxerPadsShortPacketsWithAnAdaptationFieldNotZeros(t *testing.T) {
	// A remux to MP4 (Frigate's annexb-to-HVCC converter, among others) folds
	// trailing zero bytes into the preceding NAL's length field instead of
	// discarding them as Annex B padding. A packet short of a full 184 bytes
	// of real payload must mark the gap with a stuffed adaptation field, not
	// leave it as the zero bytes Go initialises a new packet to.
	m, err := NewMuxer("h264")
	if err != nil {
		t.Fatal(err)
	}
	// A 500 byte frame is short enough to leave its last packet well short
	// of a full 184 bytes of payload.
	payload := make([]byte, 500)
	for i := range payload {
		payload[i] = 0xAB // never zero, so any trailing zero found is padding
	}
	out := m.Frame(frame(baichuan.FrameIFrame, 1000, payload...))

	var lastVideoPkt []byte
	for i := 0; i+PacketSize <= len(out); i += PacketSize {
		p := out[i : i+PacketSize]
		pid := PID(p[1]&0x1F)<<8 | PID(p[2])
		if pid == PIDVideo {
			lastVideoPkt = p
		}
	}
	if lastVideoPkt == nil {
		t.Fatal("no video packets emitted")
	}
	afc := (lastVideoPkt[3] >> 4) & 0x3
	if afc != 0x3 {
		t.Fatalf("last packet has adaptation_field_control %#x, want 0x3 (adaptation field and payload)", afc)
	}
	afLen := int(lastVideoPkt[4])
	if afLen == 0 {
		t.Fatal("last packet's adaptation field is empty, want it stretched to pad the packet")
	}
	// Every byte the adaptation field claims, after its own one-byte flags
	// field, must be stuffing (0xFF), never a leftover zero.
	for i := 6; i < 5+afLen; i++ {
		if lastVideoPkt[i] != 0xFF {
			t.Errorf("adaptation field byte %d = %#x, want stuffing 0xFF", i, lastVideoPkt[i])
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

func TestMuxerRepeatsTablesEveryFewHundredMilliseconds(t *testing.T) {
	// Every 100 frames, the previous interval, is 4 to 7 seconds at real
	// camera frame rates: long enough that a client tuning in mid stream
	// visibly waits before it can decode anything. This asserts the gap
	// between repeats stays under one second at 25fps (40ms per frame), a
	// generous margin over the 500ms target.
	m, _ := NewMuxer("h264")
	const frameStepMicros = 40000
	var lastPMTFrame, maxGapFrames int
	for i := 0; i < 200; i++ {
		out := m.Frame(frame(baichuan.FrameIFrame, uint32(i)*frameStepMicros, 1, 2, 3))
		for j := 0; j < len(out); j += PacketSize {
			pid := PID(out[j+1]&0x1F)<<8 | PID(out[j+2])
			if pid == PIDPMT {
				if gap := i - lastPMTFrame; i > 0 && gap > maxGapFrames {
					maxGapFrames = gap
				}
				lastPMTFrame = i
			}
		}
	}
	const oneSecondOfFrames = 1000 / (frameStepMicros / 1000)
	if maxGapFrames > oneSecondOfFrames {
		t.Fatalf("largest gap between PAT/PMT repeats was %d frames (%dms), want under %dms",
			maxGapFrames, maxGapFrames*frameStepMicros/1000, oneSecondOfFrames*frameStepMicros/1000)
	}
}

func TestMuxerPCRCadenceStaysUnderTheTSTDLimit(t *testing.T) {
	// ISO 13818-1's T-STD model requires a PCR at least every 100ms on the
	// PID that carries one. Tying it to keyframes alone happened to satisfy
	// this only because the test capture runs near 25fps; a slower camera
	// would miss it, so the PCR must be paced off the clock, not frame kind.
	m, _ := NewMuxer("h264")
	const frameStepMicros = 40000
	var lastPCRPTS uint64
	var sawPCR bool
	var maxGapTicks uint64
	for i := 0; i < 50; i++ {
		out := m.Frame(frame(baichuan.FrameIFrame, uint32(i)*frameStepMicros, 1, 2, 3))
		for j := 0; j+PacketSize <= len(out); j += PacketSize {
			p := out[j : j+PacketSize]
			pid := PID(p[1]&0x1F)<<8 | PID(p[2])
			if pid != PIDVideo || (p[3]>>4)&0x3 != 0x2 {
				continue
			}
			pcr := pcrFromAdaptationField(p)
			if sawPCR {
				if gap := pcr - lastPCRPTS; gap > maxGapTicks {
					maxGapTicks = gap
				}
			}
			lastPCRPTS = pcr
			sawPCR = true
		}
	}
	if !sawPCR {
		t.Fatal("no PCR packets found")
	}
	// The muxer only gets a chance to emit a PCR when a frame arrives, so the
	// realistic bound is the target interval plus one frame period, not the
	// target interval alone. What must hold is that this stays under the
	// T-STD hard limit of 9000 ticks (100ms), with room to spare.
	const frameStepTicks = frameStepMicros * 9 / 100
	const wantMax = pcrIntervalTicks + frameStepTicks
	if maxGapTicks > wantMax {
		t.Fatalf("largest gap between PCRs was %d ticks, want at most %d", maxGapTicks, wantMax)
	}
	const tstdLimitTicks = 9000
	if wantMax >= tstdLimitTicks {
		t.Fatalf("target cadence plus one frame period is %d ticks, want it under the T-STD limit of %d", wantMax, tstdLimitTicks)
	}
}

func TestMuxerPCRLagsPTSByAFixedDelay(t *testing.T) {
	// A PCR equal to its PTS asserts a decoder buffer with zero fill time. A
	// T-STD strict client can reject that; ffmpeg does not, which is why this
	// was not caught by the ffprobe capture test.
	m, _ := NewMuxer("h264")
	// The very first frame always rebases to PTS 0, which would make a
	// delayed PCR clamp to 0 too and hide the thing under test. Use the
	// second frame instead, 200ms later so it clears pcrIntervalTicks and
	// gets a PCR of its own.
	m.Frame(frame(baichuan.FrameIFrame, 1000000, 1, 2, 3))
	out := m.Frame(frame(baichuan.FrameIFrame, 1200000, 1, 2, 3))

	var pcr uint64
	var sawPCR bool
	for j := 0; j+PacketSize <= len(out); j += PacketSize {
		p := out[j : j+PacketSize]
		pid := PID(p[1]&0x1F)<<8 | PID(p[2])
		if pid == PIDVideo && (p[3]>>4)&0x3 == 0x2 {
			pcr = pcrFromAdaptationField(p)
			sawPCR = true
		}
	}
	pts, ok := findPTS(out)
	if !sawPCR || !ok {
		t.Fatal("could not find both a PCR and a PTS in the muxer's output")
	}
	if pts-pcr != pcrDecodeDelayTicks {
		t.Fatalf("PTS - PCR = %d ticks, want exactly %d", pts-pcr, pcrDecodeDelayTicks)
	}
}

// pcrFromAdaptationField decodes the 33 bit PCR base out of an adaptation
// field carrying one, discarding the 9 bit extension this muxer never sets.
func pcrFromAdaptationField(p []byte) uint64 {
	f := p[6:12]
	return uint64(f[0])<<25 | uint64(f[1])<<17 | uint64(f[2])<<9 | uint64(f[3])<<1 | uint64(f[4]>>7)
}

func TestMuxerVideoContinuityHasNoGaps(t *testing.T) {
	// A packet with no payload (adaptation_field_control 2, the PCR-only
	// packets ahead of each keyframe) must not consume a new continuity
	// counter value: ISO 13818-1 says the counter only advances on packets
	// that carry payload. A muxer that advances it anyway opens a gap that a
	// strict demuxer reports as a corrupt packet on every single keyframe,
	// which is exactly what happened here before this test existed.
	m, _ := NewMuxer("h264")
	var out []byte
	for i := 0; i < 20; i++ {
		out = append(out, m.Frame(frame(baichuan.FrameIFrame, uint32(i)*40000, 1, 2, 3))...)
	}

	seen := map[PID]bool{}
	var expected map[PID]byte
	expected = map[PID]byte{}
	for i := 0; i+PacketSize <= len(out); i += PacketSize {
		p := out[i : i+PacketSize]
		pid := PID(p[1]&0x1F)<<8 | PID(p[2])
		if pid != PIDVideo {
			continue
		}
		afc := (p[3] >> 4) & 0x3
		cc := p[3] & 0x0F
		if afc == 0x2 || afc == 0x0 {
			continue // no payload: the counter is not required to advance here
		}
		if seen[pid] {
			if cc != expected[pid] {
				t.Fatalf("continuity gap on PID %#x: got %d, want %d", pid, cc, expected[pid])
			}
		}
		seen[pid] = true
		expected[pid] = (cc + 1) & 0x0F
	}
	if !seen[PIDVideo] {
		t.Fatal("no video payload packets found")
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
		// A small frame's only packet can carry a stuffed adaptation field
		// ahead of the PES header, when its payload does not fill the whole
		// packet: skip it the same way a real demuxer would.
		off := 4
		if p[3]&0x20 != 0 {
			off += 1 + int(p[4])
		}
		pes := p[off:]
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

func TestPMTDeclaresBothStreamsWhenAudioIsPresent(t *testing.T) {
	m, err := NewMuxerWithAudio("h265", "aac")
	if err != nil {
		t.Fatal(err)
	}
	h := m.Header()
	// The PMT must carry an entry for each of the two elementary streams.
	if !containsSeq(h, []byte{StreamTypeHEVC, 0xE0 | byte(PIDVideo>>8), byte(PIDVideo & 0xFF)}) {
		t.Error("PMT does not declare the video stream")
	}
	if !containsSeq(h, []byte{StreamTypeAAC, 0xE0 | byte(PIDAudio>>8), byte(PIDAudio & 0xFF)}) {
		t.Error("PMT does not declare the audio stream")
	}
}

func TestAudioFramesAreMuxedOnTheirOwnPID(t *testing.T) {
	m, _ := NewMuxerWithAudio("h264", "aac")
	m.Frame(frame(baichuan.FrameIFrame, 1000, 1, 2, 3))
	out := m.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Codec: "aac", Micros: 2000, Data: []byte{0xFF, 0xF1, 0, 0}})
	if len(out) == 0 {
		t.Fatal("audio frame produced no packets")
	}
	var sawAudio bool
	for i := 0; i < len(out); i += PacketSize {
		if PID(out[i+1]&0x1F)<<8|PID(out[i+2]) == PIDAudio {
			sawAudio = true
		}
	}
	if !sawAudio {
		t.Fatal("audio was not carried on the audio PID")
	}
}

func TestAudioIsDroppedWhenTheMuxerIsVideoOnly(t *testing.T) {
	m, _ := NewMuxer("h264")
	m.Frame(frame(baichuan.FrameIFrame, 1000, 1))
	if got := m.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Micros: 2000, Data: []byte{1, 2}}); len(got) != 0 {
		t.Fatal("a video only muxer emitted audio")
	}
}

func TestADPCMIsDroppedRatherThanMuxedAsAAC(t *testing.T) {
	// Some cameras emit ADPCM, which has no MPEG-TS stream type here. It is
	// reported and dropped, never relabelled: transcoding would put ffmpeg
	// back in the serving path, which is a non-goal.
	m, _ := NewMuxerWithAudio("h264", "aac")
	m.Frame(frame(baichuan.FrameIFrame, 1000, 1))
	if got := m.Frame(baichuan.Frame{Kind: baichuan.FrameADPCM, Micros: 2000, Data: []byte{1, 2}}); len(got) != 0 {
		t.Fatal("ADPCM was muxed; it should be dropped and counted")
	}
	if m.DroppedAudio() == 0 {
		t.Fatal("dropped ADPCM was not counted")
	}
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
	// The capture is known to carry 56 AAC frames alongside its video (see
	// TestDepacketiserOnH265Capture's audio= count). Muxing with audio here,
	// rather than NewMuxer's video-only form, is the whole point: production
	// lost audio silently for days because a video-only path never surfaces
	// that a second elementary stream even exists. If a future capture has
	// none, this falls back to a video-only assertion and says why in the
	// skip log rather than asserting nothing.
	m, err := NewMuxerWithAudio("h265", "aac")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	out.Write(m.Header())

	var fedVideoFrames, fedAudioFrames int
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
			switch f.Kind {
			case baichuan.FrameIFrame, baichuan.FramePFrame:
				out.Write(m.Frame(f))
				fedVideoFrames++
			case baichuan.FrameAAC:
				out.Write(m.Frame(f))
				fedAudioFrames++
			}
		}
	}
	if fedVideoFrames == 0 {
		t.Fatal("no video frames decoded from the capture")
	}
	if fedAudioFrames == 0 {
		t.Skip("capture has no AAC frames; cannot assert a second elementary stream from it")
	}
	if m.DroppedAudio() != 0 {
		t.Errorf("DroppedAudio = %d, want 0: every frame fed here is video or AAC", m.DroppedAudio())
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

	// -v error was used here previously and hid the exact class of error
	// this test exists to catch: warning level is what surfaces a demuxer
	// silently unable to find or decode the second stream.
	cmd := exec.Command("ffprobe",
		"-v", "warning",
		"-count_frames",
		"-show_entries", "stream=index,codec_type,codec_name,nb_read_frames",
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
	t.Logf("ffprobe output:\n%s", stdout.String())

	var sawVideo, sawAudio bool
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		fields := strings.Split(l, ",")
		if len(fields) != 4 {
			t.Fatalf("unexpected ffprobe line: %q", l)
		}
		// ffprobe's csv writer emits struct fields alphabetically regardless
		// of the order given to -show_entries, hence codec_name before
		// codec_type here.
		codecName, codecType, nbReadFrames := fields[1], fields[2], fields[3]
		got, err := strconv.Atoi(nbReadFrames)
		if err != nil {
			t.Fatalf("parse nb_read_frames %q: %v", nbReadFrames, err)
		}
		switch codecType {
		case "video":
			sawVideo = true
			if codecName != "hevc" {
				t.Errorf("video codec_name = %q, want hevc", codecName)
			}
			if got != fedVideoFrames {
				t.Errorf("ffprobe read %d video frames, muxer was fed %d", got, fedVideoFrames)
			}
		case "audio":
			sawAudio = true
			if codecName != "aac" {
				t.Errorf("audio codec_name = %q, want aac", codecName)
			}
			if got == 0 {
				t.Error("ffprobe read 0 audio frames, want the track's frame count nonzero")
			}
		}
	}
	if !sawVideo {
		t.Error("ffprobe reported no video stream")
	}
	if !sawAudio {
		t.Error("ffprobe reported no audio stream: a joining client cannot decode the audio track")
	}
}

func TestAudioWaitsForTablesBeforeGoingOut(t *testing.T) {
	// A direct caller that feeds audio before ever feeding video would
	// otherwise get audio PES packets with no PAT/PMT ahead of them: nothing
	// says what stream_id 0xC0 belongs to yet. internal/stream never
	// triggers this (it always builds a muxer from a video frame, and that
	// same call sends the tables before returning), but ts.Muxer must not
	// rely on that.
	m, err := NewMuxerWithAudio("h264", "aac")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Codec: "aac", Micros: 1000, Data: []byte{0xFF, 0xF1, 0, 0}}); len(got) != 0 {
		t.Fatal("audio was emitted before any tables had gone out")
	}
	// Once a video keyframe has gone out, tables exist and audio is carried.
	m.Frame(frame(baichuan.FrameIFrame, 2000, 1, 2, 3))
	if got := m.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Codec: "aac", Micros: 3000, Data: []byte{0xFF, 0xF1, 0, 0}}); len(got) == 0 {
		t.Fatal("audio was not carried once tables had gone out")
	}
}
