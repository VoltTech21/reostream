package mp4read

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/VoltTech21/reostream/internal/fakeclip"
)

// Both codecs a Reolink camera writes: every sample comes back as Annex-B,
// keyframes led by the parameter sets.
func TestReadsBothCodecs(t *testing.T) {
	for file, codec := range map[string]string{"testdata/h264.mp4": "H264", "testdata/h265.mp4": "H265"} {
		tr, err := Open(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if tr.Codec != codec || len(tr.Samples) != 30 || tr.Timescale == 0 {
			t.Fatalf("%s: codec %s, %d samples, timescale %d", file, tr.Codec, len(tr.Samples), tr.Timescale)
		}
		if !tr.Samples[0].Key || tr.LastKey() != 15 {
			t.Fatalf("%s: keyframes at 0 and 15 expected, last key %d", file, tr.LastKey())
		}
		key, err := tr.AnnexB(0)
		if err != nil {
			t.Fatal(err)
		}
		params := len(tr.Params)
		if got := bytes.Count(key, []byte{0, 0, 0, 1}); got < params+1 {
			t.Fatalf("%s: keyframe has %d NALs, want parameter sets (%d) and the picture", file, got, params)
		}
		for i := range tr.Samples {
			if _, err := tr.AnnexB(i); err != nil {
				t.Fatalf("%s sample %d: %v", file, i, err)
			}
		}
		tr.Close()
	}
}

// A fragmented file like the battery doorbell's: the moov has empty sample
// tables, and the HEVC and AAC samples sit in moof+mdat pairs.
// Box builders for the synthetic files below.

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func mk(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	return append(append(be32(uint32(8+len(body))), typ...), body...)
}

func full(typ string, flags uint32, parts ...[]byte) []byte {
	return mk(typ, append([][]byte{be32(flags)}, parts...)...)
}

func u32s(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = append(b, be32(v)...)
	}
	return b
}

// nal is a sample of one length-prefixed NAL carrying a marker byte.
func nal(mark byte, n int) []byte {
	return append(be32(uint32(n)), bytes.Repeat([]byte{mark}, n)...)
}

// trakBox is a track with the given sample entry and sample tables; edts, if
// any, goes in front of mdia.
func trakBox(id, scale uint32, entry []byte, tables [][]byte, edts ...[]byte) []byte {
	stbl := mk("stbl", append([][]byte{full("stsd", 0, be32(1), entry)}, tables...)...)
	return mk("trak", append(append([][]byte{full("tkhd", 3, u32s(0, 0, id, 0, 0))}, edts...),
		mk("mdia", full("mdhd", 0, u32s(0, 0, scale, 0, 0)), mk("minf", stbl)))...)
}

// emptyTables are a fragmented file's sample tables: no samples.
var emptyTables = [][]byte{
	full("stts", 0, be32(0)),
	full("stsc", 0, be32(0)),
	full("stsz", 0, u32s(0, 0)),
	full("stco", 0, be32(0)),
}

// mp4a is an AAC sample entry whose esds carries asc.
func mp4a(asc []byte) []byte {
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	// A DecoderConfigDescriptor with the long (four byte) length form
	// encoders write.
	dcd := append([]byte{0x04, 0x80, 0x80, 0x80, byte(13 + len(dsi)), 0x40, 0x15}, make([]byte, 11)...)
	dcd = append(dcd, dsi...)
	es := append([]byte{0x03, byte(3 + len(dcd) + 3), 0, 1, 0}, dcd...)
	es = append(es, 0x06, 1, 2) // SLConfigDescriptor
	entry := make([]byte, 28)
	entry[7] = 1                               // data_reference_index
	binary.BigEndian.PutUint16(entry[16:], 1)  // channelcount
	binary.BigEndian.PutUint16(entry[18:], 16) // samplesize
	binary.BigEndian.PutUint32(entry[24:], 16000<<16)
	return mk("mp4a", entry, full("esds", 0, es))
}

// lc16kMono is the AudioSpecificConfig for AAC-LC, 16 kHz, mono: object
// type 2, rate index 8, one channel.
var lc16kMono = []byte{0x14, 0x08}

func TestReadsFragmented(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0c}
	hvcC := make([]byte, 23)
	hvcC[21], hvcC[22] = 3, 1
	hvcC = append(hvcC, 0x20, 0, 1, 0, byte(len(vps)))
	hvcC = append(hvcC, vps...)
	video := trakBox(1, 15000, mk("hvc1", make([]byte, 78), mk("hvcC", hvcC)), emptyTables)
	audio := trakBox(2, 16000, mp4a(lc16kMono), emptyTables)
	const nonSync = 0x10000
	moov := mk("moov", video, audio, mk("mvex",
		full("trex", 0, u32s(1, 1, 1000, 0, nonSync)),
		full("trex", 0, u32s(2, 1, 1024, 0, 0))))

	// Fragment 1: base is the moof, explicit data offsets; video then audio.
	v1 := [][]byte{nal(1, 20), nal(2, 7), nal(3, 9)}
	a1 := [][]byte{{0xa1, 0xa1}, {0xa2, 0xa2, 0xa2}}
	moof1 := func(dataOff uint32) []byte {
		vo := dataOff
		ao := vo + uint32(len(bytes.Join(v1, nil)))
		return mk("moof",
			full("mfhd", 0, be32(1)),
			mk("traf",
				full("tfhd", 0x20000, be32(1)),
				full("tfdt", 0, be32(0)),
				full("trun", 0x1|0x4|0x200, u32s(3, vo, 0, 24, 11, 13))),
			mk("traf",
				full("tfhd", 0x20000, be32(2)),
				full("tfdt", 0, be32(160)),
				full("trun", 0x1|0x200, u32s(2, ao, 2, 3))))
	}
	m1 := moof1(0)
	m1 = moof1(uint32(len(m1) + 8))
	mdat1 := mk("mdat", bytes.Join(append(append([][]byte{}, v1...), a1...), nil))

	// Fragment 2: no default-base-is-moof, so the first traf counts from
	// the moof and the audio traf follows the video's data; per-sample
	// durations and flags; a gap before its tfdt.
	v2 := [][]byte{nal(4, 30), nal(5, 5)}
	moof2 := func(dataOff uint32) []byte {
		return mk("moof",
			full("mfhd", 0, be32(2)),
			mk("traf",
				full("tfhd", 0, be32(1)),
				full("tfdt", 0, be32(3500)),
				full("trun", 0x1|0x100|0x200|0x400, u32s(2, dataOff, 1000, 34, 0, 1200, 9, nonSync))),
			mk("traf",
				full("tfhd", 0x10, u32s(2, 4)),
				full("trun", 0, be32(1))))
	}
	m2 := moof2(0)
	m2 = moof2(uint32(len(m2) + 8))
	mdat2 := mk("mdat", bytes.Join(v2, nil), []byte{0xa3, 0xa3, 0xa3, 0xa3})

	path := filepath.Join(t.TempDir(), "frag.mp4")
	file := bytes.Join([][]byte{mk("ftyp", []byte("iso4"), be32(0), []byte("iso4hvc1")), moov, m1, mdat1, m2, mdat2}, nil)
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.Codec != "H265" || tr.Timescale != 15000 || len(tr.Params) != 1 {
		t.Fatalf("codec %s, timescale %d, %d params", tr.Codec, tr.Timescale, len(tr.Params))
	}
	want := []struct {
		mark byte
		size uint32
		dur  uint32
		key  bool
	}{{1, 24, 1000, true}, {2, 11, 1000, false}, {3, 13, 1500, false}, {4, 34, 1000, true}, {5, 9, 1200, false}}
	if len(tr.Samples) != len(want) {
		t.Fatalf("%d samples, want %d", len(tr.Samples), len(want))
	}
	var at uint32
	starts := []uint32{0, 1000, 2000, 3500, 4500}
	for i, w := range want {
		s := tr.Samples[i]
		if s.Size != w.size || s.Dur != w.dur || s.Key != w.key || at != starts[i] {
			t.Fatalf("sample %d: size %d dur %d key %v at %d, want %+v at %d", i, s.Size, s.Dur, s.Key, at, w, starts[i])
		}
		at += s.Dur
		es, err := tr.AnnexB(i)
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		pic := es[bytes.LastIndex(es, []byte{0, 0, 0, 1})+4:]
		if int(w.size)-4 != len(pic) || bytes.Count(pic, []byte{w.mark}) != len(pic) {
			t.Fatalf("sample %d: read %x, want marker %d", i, pic, w.mark)
		}
		if w.key != bytes.Contains(es, vps) {
			t.Fatalf("sample %d: parameter sets present %v, want %v", i, !w.key, w.key)
		}
	}
	if tr.LastKey() != 3 {
		t.Fatalf("last key %d, want 3", tr.LastKey())
	}

	// The audio: the first fragment's two samples, 10 ms in (its tfdt),
	// then the second's one, sized by its tfhd and timed by its trex.
	a := tr.Audio
	if a == nil {
		t.Fatal("no audio track")
	}
	if a.Object != 2 || a.Rate != 16000 || a.Channels != 1 || a.Timescale != 16000 || a.Start != 160 {
		t.Fatalf("audio object %d, %d Hz, %d channels, timescale %d, start %d", a.Object, a.Rate, a.Channels, a.Timescale, a.Start)
	}
	if tr.Start != 0 {
		t.Fatalf("video starts at %d, want 0", tr.Start)
	}
	wantAudio := [][]byte{{0xa1, 0xa1}, {0xa2, 0xa2, 0xa2}, {0xa3, 0xa3, 0xa3, 0xa3}}
	if len(a.Samples) != len(wantAudio) {
		t.Fatalf("%d audio samples, want %d", len(a.Samples), len(wantAudio))
	}
	for i, w := range wantAudio {
		if a.Samples[i].Dur != 1024 {
			t.Fatalf("audio sample %d lasts %d, want 1024", i, a.Samples[i].Dur)
		}
		adts, err := a.ADTS(i)
		if err != nil {
			t.Fatalf("audio sample %d: %v", i, err)
		}
		if !bytes.Equal(adts[7:], w) {
			t.Fatalf("audio sample %d: read %x, want %x", i, adts[7:], w)
		}
	}
}

// A plain (progressive) file with an AAC track: samples from stsz, stco
// and stsc, durations from stts, and the edit list's skip of the
// encoder's priming samples in Start. Each comes back as one ADTS frame.
func TestReadsAACFromSampleTables(t *testing.T) {
	avcC := []byte{1, 0x42, 0, 0x1e, 0xff, 0xe1, 0, 3, 0x67, 0x42, 0, 1, 0, 2, 0x68, 0xce}
	// The mdat: three video samples, then five audio ones in two chunks.
	vs := [][]byte{nal(1, 10), nal(2, 6), nal(3, 6)}
	as := [][]byte{
		bytes.Repeat([]byte{0xa1}, 5), bytes.Repeat([]byte{0xa2}, 7), bytes.Repeat([]byte{0xa3}, 4),
		bytes.Repeat([]byte{0xa4}, 9), bytes.Repeat([]byte{0xa5}, 3),
	}
	file := func(mdatAt uint32) []byte {
		vat := mdatAt + 8
		aat := vat + uint32(len(bytes.Join(vs, nil)))
		bat := aat + uint32(len(bytes.Join(as[:3], nil)))
		video := trakBox(1, 15000, mk("avc1", make([]byte, 78), mk("avcC", avcC)), [][]byte{
			full("stts", 0, u32s(1, 3, 1000)),
			full("stss", 0, u32s(1, 1)),
			full("stsc", 0, u32s(1, 1, 3, 1)),
			full("stsz", 0, u32s(0, 3, 14, 10, 10)),
			full("stco", 0, u32s(1, vat)),
		})
		audio := trakBox(2, 16000, mp4a(lc16kMono), [][]byte{
			full("stts", 0, u32s(1, 5, 1024)),
			full("stsc", 0, u32s(2, 1, 3, 1, 2, 2, 1)),
			full("stsz", 0, u32s(0, 5, 5, 7, 4, 9, 3)),
			full("stco", 0, u32s(2, aat, bat)),
		}, mk("edts", full("elst", 0, u32s(1, 4000, 1024, 0x10000))))
		moov := mk("moov", full("mvhd", 0, u32s(0, 0, 1000, 4000)), audio, video)
		head := append(mk("ftyp", []byte("isom"), be32(0), []byte("isomavc1")), moov...)
		if mdatAt == 0 {
			return head
		}
		return append(head, mk("mdat", bytes.Join(vs, nil), bytes.Join(as, nil))...)
	}
	path := filepath.Join(t.TempDir(), "plain.mp4")
	if err := os.WriteFile(path, file(uint32(len(file(0)))), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.Codec != "H264" || len(tr.Samples) != 3 || tr.Start != 0 {
		t.Fatalf("video %s, %d samples, start %d", tr.Codec, len(tr.Samples), tr.Start)
	}
	a := tr.Audio
	if a == nil {
		t.Fatal("no audio track")
	}
	if a.Rate != 16000 || a.Channels != 1 || a.Start != -1024 || len(a.Samples) != 5 {
		t.Fatalf("audio %d Hz, %d channels, start %d, %d samples", a.Rate, a.Channels, a.Start, len(a.Samples))
	}
	for i, w := range as {
		if a.Samples[i].Dur != 1024 {
			t.Fatalf("audio sample %d lasts %d", i, a.Samples[i].Dur)
		}
		adts, err := a.ADTS(i)
		if err != nil {
			t.Fatal(err)
		}
		// AAC-LC (profile 1), 16 kHz (index 8), one channel, the frame
		// length with the header, one raw data block.
		n := len(w) + 7
		hdr := []byte{0xff, 0xf1, 1<<6 | 8<<2, 1<<6 | byte(n>>11), byte(n >> 3), byte(n&7)<<5 | 0x1f, 0xfc}
		if !bytes.Equal(adts[:7], hdr) || !bytes.Equal(adts[7:], w) {
			t.Fatalf("audio sample %d: %x, want %x then %x", i, adts, hdr, w)
		}
	}
}

// A file with no audio track reads as before, with no Audio.
func TestNoAudioTrack(t *testing.T) {
	tr, err := Open("testdata/h264.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if tr.Audio != nil {
		t.Fatalf("audio %+v in a file with none", tr.Audio)
	}
}

// What ADTS can and cannot frame.
func TestAudioSpecificConfigs(t *testing.T) {
	for _, c := range []struct {
		name          string
		asc           []byte
		ok            bool
		obj, rate, ch int
	}{
		{"AAC-LC 16 kHz mono", []byte{0x14, 0x08}, true, 2, 16000, 1},
		{"AAC-LC 48 kHz stereo", []byte{0x11, 0x90}, true, 2, 48000, 2},
		// SBR (5) at 24 kHz core over 48 kHz, on AAC-LC: framed as the core.
		{"HE-AAC", []byte{0x2b, 0x09, 0x88, 0x00}, true, 2, 24000, 1},
		{"explicit rate", []byte{0x17, 0x80, 0x3e, 0x80, 0x08}, false, 0, 0, 0},
		{"program config element", []byte{0x14, 0x00}, false, 0, 0, 0},
		{"object type ADTS has no profile for", []byte{0x3c, 0x08}, false, 0, 0, 0},
		{"truncated", []byte{0x14}, false, 0, 0, 0},
	} {
		a, ok := parseASC(c.asc)
		if ok != c.ok {
			t.Errorf("%s: ok %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && (a.Object != c.obj || a.Rate != c.rate || a.Channels != c.ch) {
			t.Errorf("%s: object %d, %d Hz, %d channels", c.name, a.Object, a.Rate, a.Channels)
		}
	}
}

// A clip with sound, plain and fragmented alike: every picture and every
// AAC frame, in order, timed, the sound starting where the file says.
func TestReadsBothLayoutsWithSound(t *testing.T) {
	for _, frag := range []bool{false, true} {
		c := fakeclip.Clip{Fragmented: frag, FPS: 15, Frames: 45, KeyEvery: 15, AudioFrames: 47, AudioDelay: 2048}
		path := filepath.Join(t.TempDir(), "clip.mp4")
		if err := os.WriteFile(path, fakeclip.Build(c), 0o644); err != nil {
			t.Fatal(err)
		}
		tr, err := Open(path)
		if err != nil {
			t.Fatalf("fragmented %v: %v", frag, err)
		}
		if len(tr.Samples) != c.Frames || tr.Start != 0 || tr.Timescale != fakeclip.VideoScale {
			t.Fatalf("fragmented %v: %d pictures, start %d, timescale %d", frag, len(tr.Samples), tr.Start, tr.Timescale)
		}
		for i, s := range tr.Samples {
			au, err := tr.AnnexB(i)
			if err != nil {
				t.Fatal(err)
			}
			if s.Key != (i%c.KeyEvery == 0) || s.Dur != fakeclip.VideoScale/15 || fakeclip.VideoIndex(au) != i {
				t.Fatalf("fragmented %v: picture %d: key %v, dur %d, reads as %d", frag, i, s.Key, s.Dur, fakeclip.VideoIndex(au))
			}
		}
		a := tr.Audio
		if a == nil {
			t.Fatalf("fragmented %v: no audio", frag)
		}
		if len(a.Samples) != c.AudioFrames || a.Start != int64(c.AudioDelay) || a.Timescale != fakeclip.AudioScale {
			t.Fatalf("fragmented %v: %d audio frames, start %d, timescale %d", frag, len(a.Samples), a.Start, a.Timescale)
		}
		for j, s := range a.Samples {
			adts, err := a.ADTS(j)
			if err != nil {
				t.Fatal(err)
			}
			if s.Dur != 1024 || fakeclip.AudioIndex(adts) != j {
				t.Fatalf("fragmented %v: audio frame %d: dur %d, reads as %d", frag, j, s.Dur, fakeclip.AudioIndex(adts))
			}
		}
		tr.Close()
	}
}
