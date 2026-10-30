package baichuan

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func buildFrame(magic []byte, hdrLen int, codec string, micros uint32, payload []byte) []byte {
	b := make([]byte, hdrLen)
	copy(b[0:], magic)
	copy(b[4:], []byte(codec))
	binary.LittleEndian.PutUint32(b[8:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(b[16:], micros)
	return append(b, payload...)
}

func TestDepacketizerEmitsAnIFrame(t *testing.T) {
	d := NewDepacketizer()
	d.Write(buildFrame(magicIFrame, 32, "H265", 1234, []byte("videodata")), true)
	f, ok := d.Next()
	if !ok {
		t.Fatal("no frame emitted")
	}
	if f.Kind != FrameIFrame {
		t.Errorf("Kind = %v, want iframe", f.Kind)
	}
	if f.Codec != "H265" {
		t.Errorf("Codec = %q, want H265", f.Codec)
	}
	if f.Micros != 1234 {
		t.Errorf("Micros = %d, want 1234", f.Micros)
	}
	if string(f.Data) != "videodata" {
		t.Errorf("Data = %q", f.Data)
	}
}

// The TrackMix zoom lens is channel 1, and its video packets say so in the
// magic's first digit: "10dc" for an I frame, "11dc" for a P frame.
func TestDepacketizerReadsChannelOneVideo(t *testing.T) {
	d := NewDepacketizer()
	for _, want := range []struct {
		magic string
		hdr   int
		kind  FrameKind
		data  string
	}{{"10dc", 32, FrameIFrame, "zoomkey"}, {"11dc", 24, FramePFrame, "zoomdelta"}} {
		d.Write(buildFrame([]byte(want.magic), want.hdr, "H264", 1, []byte(want.data)), true)
		f, ok := d.Next()
		if !ok {
			t.Fatalf("no %v emitted", want.kind)
		}
		if f.Kind != want.kind || f.Codec != "H264" || string(f.Data) != want.data {
			t.Errorf("got %v %q %q, want %v H264 %q", f.Kind, f.Codec, f.Data, want.kind, want.data)
		}
	}
}

// A media packet may straddle message boundaries.
func TestDepacketizerWaitsForASplitFrame(t *testing.T) {
	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i)
	}
	full := buildFrame(magicIFrame, 32, "H264", 7, payload)

	d := NewDepacketizer()
	d.Write(full[:1000], true)
	if _, ok := d.Next(); ok {
		t.Fatal("emitted a frame before all of it arrived")
	}
	d.Write(full[1000:], false)
	f, ok := d.Next()
	if !ok {
		t.Fatal("no frame after the rest arrived")
	}
	if !bytes.Equal(f.Data, payload) {
		t.Errorf("Data = %d bytes, want %d", len(f.Data), len(payload))
	}
}

// The real proof: decode the captured stream end to end.
func TestDepacketizerOnRealCapture(t *testing.T) {
	b := loadFixture(t, "stream_s2c.bin")
	r := NewReader(bytes.NewReader(b))
	d := NewDepacketizer()

	var iframes, pframes, audio int
	var lastMicros uint32
	var codec string
	var info Frame

	for i := 0; ; i++ {
		m, err := r.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if i == 1 {
			r.SetAESKey(AESKey(fixtureNonce, fixturePassword))
		}
		if len(m.Payload) == 0 {
			continue
		}
		d.Write(m.Payload, m.StartsPacket)
		for {
			f, ok := d.Next()
			if !ok {
				break
			}
			switch f.Kind {
			case FrameIFrame:
				iframes++
			case FramePFrame:
				pframes++
			case FrameAAC, FrameADPCM:
				audio++
			case FrameInfo:
				info = f
			}
			if f.Kind == FrameIFrame || f.Kind == FramePFrame {
				if f.Codec != "" {
					codec = f.Codec
				}
				// Timestamps must advance; the muxer depends on it.
				if lastMicros != 0 && f.Micros < lastMicros {
					t.Errorf("timestamps went backwards: %d after %d", f.Micros, lastMicros)
				}
				lastMicros = f.Micros
			}
		}
	}

	t.Logf("iframes=%d pframes=%d audio=%d codec=%s info=%dx%d@%dfps skipped=%d",
		iframes, pframes, audio, codec, info.Width, info.Height, info.FPS, d.Skipped())

	if iframes == 0 {
		t.Error("no I frames decoded from the capture")
	}
	if pframes == 0 {
		t.Error("no P frames decoded from the capture")
	}
	if codec != "H264" && codec != "H265" {
		t.Errorf("codec = %q, want H264 or H265", codec)
	}
	if d.Skipped() > 0 {
		t.Errorf("resynchronized %d times; the stream should parse cleanly", d.Skipped())
	}
}

// The size field counts the coded picture only. Cameras that put metadata
// between the packet header and the first NAL start code make the packet
// hdr+prefix+size bytes long, and consuming hdr+size instead cut the last
// prefix bytes off every frame: on the fisheye that was 104 to 184 bytes,
// enough to corrupt the bottom macroblock rows of every picture while the
// stream otherwise looked healthy.
func TestDepacketizerAccountsForAFramePrefix(t *testing.T) {
	prefix := bytes.Repeat([]byte{0xAB}, 104)
	// 0x65 is an IDR slice header and 0x41 a non-IDR one. The picture bytes
	// have to begin with a NAL header the parser accepts: it now validates
	// one before treating a start code as the end of the prefix, so plain
	// text after the start code is correctly not a picture.
	picture := append([]byte{0, 0, 0, 1, 0x65}, []byte("first picture")...)
	second := append([]byte{0, 0, 0, 1, 0x41}, []byte("second picture")...)

	// The size field covers the picture, not the prefix, so build the header
	// from the picture alone and splice the prefix in behind it.
	first := buildFrame(magicIFrame, 32, "H264", 1, picture)
	first = append(first[:32], append(prefix, picture...)...)

	d := NewDepacketizer()
	d.Write(append(first, buildFrame(magicPFrame, 24, "H264", 2, second)...), true)

	f, ok := d.Next()
	if !ok {
		t.Fatal("no frame emitted")
	}
	if string(f.Data) != string(picture) {
		t.Errorf("Data = %q, want %q", f.Data, picture)
	}

	// The next packet must still parse, which it only can if the prefix was
	// counted when the first one was consumed.
	f, ok = d.Next()
	if !ok {
		t.Fatal("no second frame emitted")
	}
	if string(f.Data) != string(second) {
		t.Errorf("second Data = %q, want %q", f.Data, second)
	}
	if d.Skipped() != 0 {
		t.Errorf("Skipped = %d, want 0", d.Skipped())
	}
}

// A video packet still short of its declared size when the next packet
// starts was cut off: it is counted lost, not as filler (#17).
func TestDepacketizerCountsACutVideoPacketAsLost(t *testing.T) {
	d := NewDepacketizer()
	pkt := make([]byte, 24+100)
	copy(pkt, "01dcH264")
	binary.LittleEndian.PutUint32(pkt[8:], 100)
	d.Write(pkt[:60], true)
	if _, ok := d.Next(); ok {
		t.Fatal("a partial packet came out as a frame")
	}
	d.Write([]byte("filler"), true)
	if d.Lost() != 1 || !d.TakeGap() || d.TakeGap() {
		t.Fatalf("lost=%d; want 1 and one gap reported once", d.Lost())
	}
	if d.Filler() != 0 {
		t.Fatalf("filler=%d; a cut packet is not filler", d.Filler())
	}
}

// The v3.0.0.136 firmware (RLC-410-5MP, RLC-422) packs whole packets back to
// back in one message, each padded with zeros to 8 bytes. Every one of them
// is a frame, not just the first.
func TestPacketsPaddedTogetherInOneMessage(t *testing.T) {
	var msg []byte
	for i := 0; i < 3; i++ {
		pkt := append([]byte{}, magicAAC...)
		pkt = append(pkt, 3, 0, 3, 0, 0xff, 0xf1, byte(i))
		for len(pkt)%8 != 0 {
			pkt = append(pkt, 0)
		}
		msg = append(msg, pkt...)
	}
	d := NewDepacketizer()
	d.Write(msg, true)
	n := 0
	for {
		f, ok := d.Next()
		if !ok {
			break
		}
		if f.Kind != FrameAAC || f.Data[2] != byte(n) {
			t.Fatalf("frame %d: kind %v data %x", n, f.Kind, f.Data)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("got %d frames, want 3", n)
	}
}

// The TrackMix zoom lens is channel 1 and sends its audio as "15wb".
func TestZoomLensAudioIsAudio(t *testing.T) {
	d := NewDepacketizer()
	d.Write([]byte{'1', '5', 'w', 'b', 3, 0, 3, 0, 0xff, 0xf1, 0x50}, true)
	f, ok := d.Next()
	if !ok || f.Kind != FrameAAC || len(f.Data) != 3 {
		t.Fatalf("got %v %v % x, want a 3-byte AAC frame", ok, f.Kind, f.Data)
	}
}
