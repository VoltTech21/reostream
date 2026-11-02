package mp4read

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
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
func TestReadsFragmented(t *testing.T) {
	be32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	mk := func(typ string, parts ...[]byte) []byte {
		body := bytes.Join(parts, nil)
		return append(append(be32(uint32(8+len(body))), typ...), body...)
	}
	full := func(typ string, flags uint32, parts ...[]byte) []byte {
		return mk(typ, append([][]byte{be32(flags)}, parts...)...)
	}
	u32s := func(vs ...uint32) []byte {
		var b []byte
		for _, v := range vs {
			b = append(b, be32(v)...)
		}
		return b
	}
	// A sample is one length-prefixed NAL carrying a marker byte.
	nal := func(mark byte, n int) []byte {
		return append(be32(uint32(n)), bytes.Repeat([]byte{mark}, n)...)
	}
	trak := func(id, scale uint32, entry []byte) []byte {
		stbl := mk("stbl",
			full("stsd", 0, be32(1), entry),
			full("stts", 0, be32(0)),
			full("stsc", 0, be32(0)),
			full("stsz", 0, u32s(0, 0)),
			full("stco", 0, be32(0)))
		return mk("trak",
			full("tkhd", 3, u32s(0, 0, id, 0, 0)),
			mk("mdia", full("mdhd", 0, u32s(0, 0, scale, 0, 0)), mk("minf", stbl)))
	}
	vps := []byte{0x40, 0x01, 0x0c}
	hvcC := make([]byte, 23)
	hvcC[21], hvcC[22] = 3, 1
	hvcC = append(hvcC, 0x20, 0, 1, 0, byte(len(vps)))
	hvcC = append(hvcC, vps...)
	video := trak(1, 15000, mk("hvc1", make([]byte, 78), mk("hvcC", hvcC)))
	audio := trak(2, 16000, mk("mp4a", make([]byte, 28)))
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
				full("tfdt", 0, be32(0)),
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
}
