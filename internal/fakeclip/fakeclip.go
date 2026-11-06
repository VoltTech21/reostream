// Package fakeclip builds small MP4 clips for tests, shaped like a battery
// camera's motion clips: H.264 video and, optionally, AAC-LC at 16 kHz
// mono, either as a plain file (sample tables in the moov, one mdat) or a
// fragmented one (an empty moov, then a moof+mdat pair per keyframe
// interval), the way a battery doorbell writes them.
//
// Each sample carries its own index, so a test can tell which one it is
// looking at: see VideoIndex and AudioIndex.
package fakeclip

import (
	"bytes"
	"encoding/binary"
)

// Clip describes the file Build makes.
type Clip struct {
	Fragmented bool
	// FPS is the picture rate; Frames pictures, a keyframe every KeyEvery.
	FPS, Frames, KeyEvery int
	// Stretch is how much longer than the rest, in VideoScale units, the
	// first picture lasts, as ffmpeg writes a fragmented file whose AAC
	// encoder primed: every picture after it starts that much later.
	Stretch int
	// AudioFrames is how many 1024 sample AAC frames there are, none for a
	// clip without sound. AudioDelay is how many samples after the first
	// picture the sound starts: an empty edit in a plain file, the first
	// tfdt in a fragmented one.
	AudioFrames, AudioDelay int
}

// The tracks' timescales: the video's, and the audio's (its sample rate).
const (
	VideoScale = 90_000
	AudioScale = 16_000
)

// The NAL types of a picture: an H.264 IDR slice, and a non-IDR one.
const (
	nalIDR    = 5
	nalNonIDR = 1
)

// VideoIndex reads which picture an Annex-B access unit is, from the
// last NAL in it (the picture; parameter sets go in front), or -1.
func VideoIndex(au []byte) int {
	i := bytes.LastIndex(au, []byte{0, 0, 0, 1})
	if i < 0 || len(au) < i+7 {
		return -1
	}
	return int(binary.BigEndian.Uint16(au[i+5:]))
}

// AudioIndex reads which AAC frame an ADTS frame is, or -1.
func AudioIndex(adts []byte) int {
	if len(adts) < 10 || adts[7] != 0xa0 {
		return -1
	}
	return int(binary.BigEndian.Uint16(adts[8:]))
}

func videoSample(i int, key bool) []byte {
	t := byte(nalNonIDR)
	if key {
		t = nalIDR
	}
	// One length-prefixed NAL: its header, the index, some body.
	nal := append([]byte{0x60 | t}, byte(i>>8), byte(i))
	nal = append(nal, bytes.Repeat([]byte{0x88}, 20)...)
	return append(be32(uint32(len(nal))), nal...)
}

func audioSample(j int) []byte {
	return append([]byte{0xa0, byte(j >> 8), byte(j)}, bytes.Repeat([]byte{0x55}, 9)...)
}

// Build makes the file.
func Build(c Clip) []byte {
	vdur := uint32(VideoScale / c.FPS)
	var vs, as [][]byte
	for i := 0; i < c.Frames; i++ {
		vs = append(vs, videoSample(i, i%c.KeyEvery == 0))
	}
	for j := 0; j < c.AudioFrames; j++ {
		as = append(as, audioSample(j))
	}
	ftyp := mk("ftyp", []byte("isom"), be32(0x200), []byte("isomiso2avc1mp41"))
	if c.Fragmented {
		return buildFragmented(c, ftyp, vdur, vs, as)
	}

	// Plain: the moov's tables point into one mdat, video then audio. Its
	// size does not depend on the offsets, so build it once to measure.
	moov := func(vat, aat uint32) []byte {
		var keys []uint32
		var sizes []uint32
		for i, s := range vs {
			if i%c.KeyEvery == 0 {
				keys = append(keys, uint32(i+1))
			}
			sizes = append(sizes, uint32(len(s)))
		}
		traks := [][]byte{trak(1, VideoScale, avc1(), [][]byte{
			full("stts", 0, u32s(2, 1, vdur+uint32(c.Stretch), uint32(len(vs)-1), vdur)),
			full("stss", 0, u32s(append([]uint32{uint32(len(keys))}, keys...)...)),
			full("stsc", 0, u32s(1, 1, uint32(len(vs)), 1)),
			full("stsz", 0, u32s(append([]uint32{0, uint32(len(vs))}, sizes...)...)),
			full("stco", 0, u32s(1, vat)),
		}, nil)}
		if len(as) > 0 {
			var asizes []uint32
			for _, s := range as {
				asizes = append(asizes, uint32(len(s)))
			}
			var edts []byte
			if c.AudioDelay > 0 {
				// An empty edit, in the movie's timescale (1000), then the
				// whole track from its start.
				edts = mk("edts", full("elst", 0, u32s(2,
					uint32(c.AudioDelay*1000/AudioScale), 0xffffffff, 0x10000,
					uint32(len(as)*1024*1000/AudioScale), 0, 0x10000)))
			}
			traks = append(traks, trak(2, AudioScale, mp4a(), [][]byte{
				full("stts", 0, u32s(1, uint32(len(as)), 1024)),
				full("stsc", 0, u32s(1, 1, uint32(len(as)), 1)),
				full("stsz", 0, u32s(append([]uint32{0, uint32(len(as))}, asizes...)...)),
				full("stco", 0, u32s(1, aat)),
			}, edts))
		}
		return mk("moov", append([][]byte{full("mvhd", 0, u32s(0, 0, 1000, 0))}, traks...)...)
	}
	vat := uint32(len(ftyp) + len(moov(0, 0)) + 8)
	aat := vat + uint32(len(bytes.Join(vs, nil)))
	return bytes.Join([][]byte{ftyp, moov(vat, aat), mk("mdat", bytes.Join(vs, nil), bytes.Join(as, nil))}, nil)
}

func buildFragmented(c Clip, ftyp []byte, vdur uint32, vs, as [][]byte) []byte {
	traks := [][]byte{trak(1, VideoScale, avc1(), emptyTables(), nil)}
	trex := [][]byte{full("trex", 0, u32s(1, 1, vdur, 0, 0x10000))}
	if len(as) > 0 {
		traks = append(traks, trak(2, AudioScale, mp4a(), emptyTables(), nil))
		trex = append(trex, full("trex", 0, u32s(2, 1, 1024, 0, 0)))
	}
	out := append(ftyp, mk("moov", append(append([][]byte{full("mvhd", 0, u32s(0, 0, 1000, 0))}, traks...), mk("mvex", trex...))...)...)

	// A fragment per keyframe interval; the audio frames that start inside
	// its time go with it, and any left over go with the last.
	seq := uint32(1)
	aj := 0
	for k := 0; k < len(vs); k += c.KeyEvery {
		n := min(c.KeyEvery, len(vs)-k)
		end := (uint64(k+n)*uint64(vdur) + uint64(c.Stretch)) * AudioScale / VideoScale // in audio samples
		an := 0
		for aj+an < len(as) && (k+n == len(vs) || uint64(c.AudioDelay+(aj+an)*1024) < end) {
			an++
		}
		v, a := vs[k:k+n], as[aj:aj+an]
		moof := func(dataOff uint32) []byte {
			var vsz []uint32
			for i, s := range v {
				// Sample flags: a keyframe's first, the rest not sync.
				fl := uint32(0x10000)
				if i == 0 {
					fl = 0
				}
				d := vdur
				if k+i == 0 {
					d += uint32(c.Stretch)
				}
				vsz = append(vsz, d, uint32(len(s)), fl)
			}
			at := uint64(k) * uint64(vdur)
			if k > 0 {
				at += uint64(c.Stretch)
			}
			trafs := [][]byte{full("mfhd", 0, be32(seq)), mk("traf",
				full("tfhd", 0x20000, be32(1)),
				full("tfdt", 1<<24, binary.BigEndian.AppendUint64(nil, at)),
				full("trun", 0x1|0x100|0x200|0x400, append(u32s(uint32(n), dataOff), u32s(vsz...)...)))}
			if len(a) > 0 {
				var asz []uint32
				for _, s := range a {
					asz = append(asz, uint32(len(s)))
				}
				trafs = append(trafs, mk("traf",
					full("tfhd", 0x20000, be32(2)),
					full("tfdt", 1<<24, binary.BigEndian.AppendUint64(nil, uint64(c.AudioDelay+aj*1024))),
					full("trun", 0x1|0x200, append(u32s(uint32(len(a)), dataOff+uint32(len(bytes.Join(v, nil)))), u32s(asz...)...))))
			}
			return mk("moof", trafs...)
		}
		m := moof(uint32(len(moof(0)) + 8))
		out = append(out, m...)
		out = append(out, mk("mdat", bytes.Join(v, nil), bytes.Join(a, nil))...)
		aj += an
		seq++
	}
	return out
}

func avc1() []byte {
	sps := []byte{0x67, 0x42, 0x00, 0x1e, 0xab}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	avcC := append([]byte{1, 0x42, 0, 0x1e, 0xff, 0xe1, 0, byte(len(sps))}, sps...)
	avcC = append(append(avcC, 1, 0, byte(len(pps))), pps...)
	entry := make([]byte, 78)
	entry[7] = 1                                // data_reference_index
	binary.BigEndian.PutUint16(entry[24:], 640) // width
	binary.BigEndian.PutUint16(entry[26:], 480) // height
	return mk("avc1", entry, mk("avcC", avcC))
}

// mp4a is the AAC-LC, 16 kHz, mono sample entry.
func mp4a() []byte {
	asc := []byte{0x14, 0x08} // object type 2, rate index 8, one channel
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	dcd := append([]byte{0x04, byte(13 + len(dsi)), 0x40, 0x15}, make([]byte, 11)...)
	dcd = append(dcd, dsi...)
	es := append([]byte{0x03, byte(3 + len(dcd) + 3), 0, 2, 0}, dcd...)
	es = append(es, 0x06, 1, 2)
	entry := make([]byte, 28)
	entry[7] = 1
	binary.BigEndian.PutUint16(entry[16:], 1)  // channelcount
	binary.BigEndian.PutUint16(entry[18:], 16) // samplesize
	binary.BigEndian.PutUint32(entry[24:], AudioScale<<16)
	return mk("mp4a", entry, full("esds", 0, es))
}

func emptyTables() [][]byte {
	return [][]byte{
		full("stts", 0, be32(0)),
		full("stsc", 0, be32(0)),
		full("stsz", 0, u32s(0, 0)),
		full("stco", 0, be32(0)),
	}
}

func trak(id, scale uint32, entry []byte, tables [][]byte, edts []byte) []byte {
	stbl := mk("stbl", append([][]byte{full("stsd", 0, be32(1), entry)}, tables...)...)
	parts := [][]byte{full("tkhd", 3, u32s(0, 0, id, 0, 0))}
	if edts != nil {
		parts = append(parts, edts)
	}
	return mk("trak", append(parts, mk("mdia", full("mdhd", 0, u32s(0, 0, scale, 0, 0)), mk("minf", stbl)))...)
}

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
