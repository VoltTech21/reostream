// Package mp4read reads the video track of the MP4 files Reolink cameras
// write (to their SD card, and over FTP): a plain moov with an avc1 or
// hvc1 track, or a fragmented file (battery doorbells) whose moov is empty
// and whose samples sit in moof+mdat pairs. Just enough to hand the samples
// back as Annex-B access units, parameter sets in front of each keyframe,
// and the AAC track beside them, if there is one, as ADTS frames.
package mp4read

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Sample is one access unit in the file.
type Sample struct {
	Offset int64
	Size   uint32
	// Dur is how long it shows, in Track.Timescale units.
	Dur uint32
	Key bool
}

// Track is a file's video track.
type Track struct {
	Codec     string // "H264" or "H265"
	Timescale uint32
	// Start is when the first sample shows, in Timescale units: its decode
	// time less the edit list's skip. It is what lines the audio up with
	// the picture.
	Start   int64
	Params  [][]byte // VPS/SPS/PPS or SPS/PPS, without start codes
	Samples []Sample
	// Audio is the file's AAC track, or nil when it has none, or one that
	// ADTS cannot frame.
	Audio  *Audio
	nalLen int
	f      *os.File
	frag   bool
	trak   trak
	// Every track's trex defaults, by track_ID, so a fragment of a track
	// not read here still says where the next one's data starts.
	trex map[uint32][3]uint32
}

// Audio is a file's AAC track.
type Audio struct {
	Timescale uint32
	// Start is when the first sample plays, in Timescale units, on the
	// same footing as Track.Start.
	Start int64
	// Object is the MPEG-4 audio object type (2 is AAC-LC), Rate the
	// sample rate in Hz, Channels the channel configuration.
	Object, Rate, Channels int
	Samples                []Sample
	rateIndex              int
	f                      *os.File
	trak                   trak
}

// trak is what reading a track's samples needs beyond the samples: a
// fragmented file's track_ID, and while reading the fragments, the decode
// time the next sample starts at.
type trak struct {
	id      uint32
	samples *[]Sample
	start   *int64
	next    uint64
	timed   bool // a tfdt or the sample tables have said where it starts
}

// Open reads path's video track. Close the track when done.
func Open(path string) (*Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &Track{f: f}
	moov, err := findTop(f, st.Size(), "moov")
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := t.parseMoov(moov); err != nil {
		f.Close()
		return nil, err
	}
	if t.frag {
		traks := []*trak{&t.trak}
		if t.Audio != nil {
			traks = append(traks, &t.Audio.trak)
		}
		if err := readFragments(f, st.Size(), traks, t.trex); err != nil {
			f.Close()
			return nil, err
		}
	}
	if t.Audio != nil && len(t.Audio.Samples) == 0 {
		t.Audio = nil
	}
	return t, nil
}

// Close closes the file.
func (t *Track) Close() error { return t.f.Close() }

// AnnexB reads sample i as Annex-B, with the parameter sets in front of a
// keyframe so a decoder can start there.
func (t *Track) AnnexB(i int) ([]byte, error) {
	s := t.Samples[i]
	buf := make([]byte, s.Size)
	if _, err := t.f.ReadAt(buf, s.Offset); err != nil {
		return nil, err
	}
	var out []byte
	if s.Key {
		for _, p := range t.Params {
			out = append(out, 0, 0, 0, 1)
			out = append(out, p...)
		}
	}
	for len(buf) >= t.nalLen {
		n := 0
		for i := 0; i < t.nalLen; i++ {
			n = n<<8 | int(buf[i])
		}
		buf = buf[t.nalLen:]
		if n > len(buf) {
			return nil, errors.New("mp4read: NAL runs past its sample")
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, buf[:n]...)
		buf = buf[n:]
	}
	return out, nil
}

// ADTS reads audio sample i with an ADTS header in front: the framing a
// raw AAC stream, and MPEG-TS, carry it in.
func (a *Audio) ADTS(i int) ([]byte, error) {
	s := a.Samples[i]
	n := int(s.Size) + 7
	if n >= 1<<13 {
		return nil, errors.New("mp4read: AAC sample too big for ADTS")
	}
	out := make([]byte, n)
	if _, err := a.f.ReadAt(out[7:], s.Offset); err != nil {
		return nil, err
	}
	// Sync word, MPEG-4, layer 0, no CRC; profile (object type less one),
	// rate index, channels; the frame length with the header; buffer
	// fullness all ones (variable rate); one raw data block.
	out[0], out[1] = 0xff, 0xf1
	out[2] = byte(a.Object-1)<<6 | byte(a.rateIndex)<<2 | byte(a.Channels>>2)
	out[3] = byte(a.Channels&3)<<6 | byte(n>>11)
	out[4] = byte(n >> 3)
	out[5] = byte(n&7)<<5 | 0x1f
	out[6] = 0xfc
	return out, nil
}

// LastKey is the index of the last keyframe, or -1.
func (t *Track) LastKey() int {
	for i := len(t.Samples) - 1; i >= 0; i-- {
		if t.Samples[i].Key {
			return i
		}
	}
	return -1
}

type box struct {
	typ  string
	body []byte
}

func findTop(r io.ReaderAt, size int64, want string) ([]byte, error) {
	var found []byte
	err := eachTop(r, size, func(typ string, off, h, n int64) (bool, error) {
		if typ != want {
			return true, nil
		}
		found = make([]byte, n-h)
		_, err := r.ReadAt(found, off+h)
		return false, err
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("mp4read: no %s box", want)
	}
	return found, nil
}

// eachTop walks the top-level boxes, handing fn each one's type, offset,
// header length and total length, until fn says stop.
func eachTop(r io.ReaderAt, size int64, fn func(typ string, off, h, n int64) (bool, error)) error {
	var off int64
	hdr := make([]byte, 16)
	for off+8 <= size {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr))
		typ := string(hdr[4:8])
		h := int64(8)
		if n == 1 {
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return err
			}
			n, h = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		} else if n == 0 {
			n = size - off
		}
		if n < h {
			return fmt.Errorf("mp4read: bad box %q", typ)
		}
		if more, err := fn(typ, off, h, n); err != nil || !more {
			return err
		}
		off += n
	}
	return nil
}

func children(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b))
		h := 8
		if n == 1 && len(b) >= 16 {
			n, h = int(binary.BigEndian.Uint64(b[8:])), 16
		} else if n == 0 {
			n = len(b)
		}
		if n < h || n > len(b) {
			break
		}
		out = append(out, box{string(b[4:8]), b[h:n]})
		b = b[n:]
	}
	return out
}

func child(b []byte, path ...string) []byte {
	for _, p := range path {
		found := false
		for _, c := range children(b) {
			if c.typ == p {
				b, found = c.body, true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return b
}

func (t *Track) parseMoov(moov []byte) error {
	u32 := binary.BigEndian.Uint32
	var movieScale uint32
	if mvhd := child(moov, "mvhd"); len(mvhd) >= 20 {
		if mvhd[0] == 1 && len(mvhd) >= 24 {
			movieScale = u32(mvhd[20:])
		} else {
			movieScale = u32(mvhd[12:])
		}
	}
	mvex := child(moov, "mvex")
	for _, x := range children(mvex) {
		if x.typ == "trex" && len(x.body) >= 24 {
			if t.trex == nil {
				t.trex = map[uint32][3]uint32{}
			}
			t.trex[u32(x.body[4:])] = [3]uint32{u32(x.body[12:]), u32(x.body[16:]), u32(x.body[20:])}
		}
	}
	video := false
	for _, c := range children(moov) {
		if c.typ != "trak" {
			continue
		}
		stbl := child(c.body, "mdia", "minf", "stbl")
		stsd := child(stbl, "stsd")
		if stbl == nil || len(stsd) < 8 {
			continue
		}
		entries := children(stsd[8:])
		if len(entries) == 0 {
			continue
		}
		e := entries[0]
		isVideo := !video && (e.typ == "avc1" || e.typ == "hvc1" || e.typ == "hev1")
		if !isVideo && (t.Audio != nil || e.typ != "mp4a") {
			continue
		}
		var scale uint32
		if mdhd := child(c.body, "mdia", "mdhd"); len(mdhd) >= 20 {
			if mdhd[0] == 1 && len(mdhd) >= 24 {
				scale = u32(mdhd[20:])
			} else {
				scale = u32(mdhd[12:])
			}
		}
		var tk *trak
		if isVideo {
			if len(e.body) < 78 {
				return errors.New("mp4read: short sample entry")
			}
			t.Timescale = scale
			cfg := children(e.body[78:])
			var err error
			if e.typ == "avc1" {
				t.Codec = "H264"
				err = t.parseAvcC(find(cfg, "avcC"))
			} else {
				t.Codec = "H265"
				err = t.parseHvcC(find(cfg, "hvcC"))
			}
			if err != nil {
				return err
			}
			video = true
			t.trak.samples, t.trak.start = &t.Samples, &t.Start
			tk = &t.trak
		} else {
			a, ok := parseMp4a(e.body)
			if !ok {
				// Audio that ADTS cannot frame is left out; the
				// picture still plays.
				continue
			}
			a.Timescale, a.f = scale, t.f
			a.trak.samples, a.trak.start = &a.Samples, &a.Start
			t.Audio = a
			tk = &a.trak
		}
		*tk.start = editStart(c.body, movieScale, scale)
		if mvex != nil {
			// Fragmented: the samples are in the moofs, unless the moov
			// holds some of its own first.
			t.frag = true
			if tkhd := child(c.body, "tkhd"); len(tkhd) >= 24 {
				if tkhd[0] == 1 && len(tkhd) >= 32 {
					tk.id = u32(tkhd[20:])
				} else {
					tk.id = u32(tkhd[12:])
				}
			}
			if stsz := child(stbl, "stsz"); len(stsz) < 12 || u32(stsz[8:]) == 0 {
				continue
			}
		}
		samples, err := parseTables(stbl)
		if err != nil {
			if isVideo {
				return err
			}
			t.Audio = nil
			continue
		}
		*tk.samples = samples
		tk.timed = true
	}
	if !video {
		return errors.New("mp4read: no H.264 or H.265 video track")
	}
	return nil
}

// editStart is when a track's first sample plays, in its own timescale,
// from its edit list: later by any empty edit in front, earlier by the
// media time the first real edit starts at. An AAC encoder's priming
// samples are skipped this way.
func editStart(trak []byte, movieScale, scale uint32) int64 {
	elst := child(trak, "edts", "elst")
	if len(elst) < 8 {
		return 0
	}
	u32 := binary.BigEndian.Uint32
	v1 := elst[0] == 1
	n := int(u32(elst[4:]))
	b := elst[8:]
	var empty int64
	for i := 0; i < n; i++ {
		var dur uint64
		var at int64
		if v1 {
			if len(b) < 20 {
				break
			}
			dur, at = binary.BigEndian.Uint64(b), int64(binary.BigEndian.Uint64(b[8:]))
			b = b[20:]
		} else {
			if len(b) < 12 {
				break
			}
			dur, at = uint64(u32(b)), int64(int32(u32(b[4:])))
			b = b[12:]
		}
		if at == -1 {
			if movieScale != 0 {
				empty += int64(dur * uint64(scale) / uint64(movieScale))
			}
			continue
		}
		return empty - at
	}
	return empty
}

// parseMp4a reads an mp4a sample entry's AudioSpecificConfig, from its
// esds, into the fields an ADTS header needs.
func parseMp4a(entry []byte) (*Audio, bool) {
	// The sample entry (8 bytes) and the audio sample entry (20); a
	// QuickTime sound description version 1 or 2 is longer.
	if len(entry) < 28 {
		return nil, false
	}
	at := 28
	switch binary.BigEndian.Uint16(entry[8:]) {
	case 1:
		at += 16
	case 2:
		at += 36
	}
	if len(entry) < at {
		return nil, false
	}
	cfg := children(entry[at:])
	esds := find(cfg, "esds")
	if esds == nil {
		esds = child(find(cfg, "wave"), "esds")
	}
	if len(esds) < 4 {
		return nil, false
	}
	asc := decoderSpecificInfo(esds[4:])
	return parseASC(asc)
}

// decoderSpecificInfo finds the DecoderSpecificInfo (tag 5) inside an
// ES_Descriptor (tag 3) and its DecoderConfigDescriptor (tag 4).
func decoderSpecificInfo(b []byte) []byte {
	desc := func(b []byte) (tag byte, body, rest []byte, ok bool) {
		if len(b) < 2 {
			return 0, nil, nil, false
		}
		tag, b = b[0], b[1:]
		n := 0
		for i := 0; i < 4; i++ {
			if len(b) == 0 {
				return 0, nil, nil, false
			}
			c := b[0]
			b = b[1:]
			n = n<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if n > len(b) {
			return 0, nil, nil, false
		}
		return tag, b[:n], b[n:], true
	}
	tag, es, _, ok := desc(b)
	if !ok || tag != 3 || len(es) < 3 {
		return nil
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 { // streamDependenceFlag
		if len(es) < 2 {
			return nil
		}
		es = es[2:]
	}
	if flags&0x40 != 0 { // URL_Flag
		if len(es) < 1 || len(es) < 1+int(es[0]) {
			return nil
		}
		es = es[1+int(es[0]):]
	}
	if flags&0x20 != 0 { // OCRstreamFlag
		if len(es) < 2 {
			return nil
		}
		es = es[2:]
	}
	for len(es) > 0 {
		tag, body, rest, ok := desc(es)
		if !ok {
			return nil
		}
		es = rest
		if tag != 4 || len(body) < 13 {
			continue
		}
		body = body[13:] // object type, stream type, buffer size, bitrates
		for len(body) > 0 {
			tag, info, rest, ok := desc(body)
			if !ok {
				return nil
			}
			if tag == 5 {
				return info
			}
			body = rest
		}
	}
	return nil
}

// adtsRates is the sampling frequency index table ADTS and the
// AudioSpecificConfig share.
var adtsRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// parseASC reads an AudioSpecificConfig. ADTS has two bits for the object
// type and no room for an explicit rate or a program config element, so a
// config that needs any of those is refused. HE-AAC (SBR, and PS on top)
// is framed as its AAC-LC core, which is how ADTS signals it.
func parseASC(b []byte) (*Audio, bool) {
	var pos int
	bits := func(n int) int {
		v := 0
		for i := 0; i < n; i++ {
			if pos/8 >= len(b) {
				return -1
			}
			v = v<<1 | int(b[pos/8]>>(7-pos%8)&1)
			pos++
		}
		return v
	}
	object := func() int {
		o := bits(5)
		if o == 31 {
			o = 32 + bits(6)
		}
		return o
	}
	obj := object()
	idx := bits(4)
	if idx == 15 {
		return nil, false
	}
	ch := bits(4)
	if obj == 5 || obj == 29 {
		if bits(4) == 15 { // the extension rate
			bits(24)
		}
		obj = object()
	}
	if obj < 1 || obj > 4 || idx < 0 || idx >= len(adtsRates) || ch < 1 || ch > 7 {
		return nil, false
	}
	return &Audio{Object: obj, Rate: adtsRates[idx], Channels: ch, rateIndex: idx}, true
}

func find(bs []box, typ string) []byte {
	for _, b := range bs {
		if b.typ == typ {
			return b.body
		}
	}
	return nil
}

func (t *Track) parseAvcC(b []byte) error {
	if len(b) < 7 {
		return errors.New("mp4read: no avcC")
	}
	t.nalLen = int(b[4]&3) + 1
	p := b[5:]
	for _, count := range []int{int(p[0] & 0x1f), -1} {
		if count == -1 {
			if len(p) < 1 {
				break
			}
			count = int(p[0])
		}
		p = p[1:]
		for i := 0; i < count; i++ {
			if len(p) < 2 {
				return errors.New("mp4read: short avcC")
			}
			n := int(binary.BigEndian.Uint16(p))
			if len(p) < 2+n {
				return errors.New("mp4read: short avcC")
			}
			t.Params = append(t.Params, p[2:2+n])
			p = p[2+n:]
		}
	}
	return nil
}

func (t *Track) parseHvcC(b []byte) error {
	if len(b) < 23 {
		return errors.New("mp4read: no hvcC")
	}
	t.nalLen = int(b[21]&3) + 1
	arrays := int(b[22])
	p := b[23:]
	for a := 0; a < arrays; a++ {
		if len(p) < 3 {
			return errors.New("mp4read: short hvcC")
		}
		count := int(binary.BigEndian.Uint16(p[1:]))
		p = p[3:]
		for i := 0; i < count; i++ {
			if len(p) < 2 {
				return errors.New("mp4read: short hvcC")
			}
			n := int(binary.BigEndian.Uint16(p))
			if len(p) < 2+n {
				return errors.New("mp4read: short hvcC")
			}
			t.Params = append(t.Params, p[2:2+n])
			p = p[2+n:]
		}
	}
	return nil
}

// parseTables reads a track's samples from its sample tables.
func parseTables(stbl []byte) ([]Sample, error) {
	u32 := binary.BigEndian.Uint32
	stsz := child(stbl, "stsz")
	if len(stsz) < 12 {
		return nil, errors.New("mp4read: no stsz")
	}
	fixed, count := u32(stsz[4:]), int(u32(stsz[8:]))
	samples := make([]Sample, count)
	for i := range samples {
		if fixed != 0 {
			samples[i].Size = fixed
		} else if 12+4*i+4 <= len(stsz) {
			samples[i].Size = u32(stsz[12+4*i:])
		}
	}
	// Chunk offsets, then which samples each chunk holds.
	var chunks []int64
	if co := child(stbl, "stco"); len(co) >= 8 {
		for i := 0; i < int(u32(co[4:])) && 8+4*i+4 <= len(co); i++ {
			chunks = append(chunks, int64(u32(co[8+4*i:])))
		}
	} else if co := child(stbl, "co64"); len(co) >= 8 {
		for i := 0; i < int(u32(co[4:])) && 8+8*i+8 <= len(co); i++ {
			chunks = append(chunks, int64(binary.BigEndian.Uint64(co[8+8*i:])))
		}
	}
	stsc := child(stbl, "stsc")
	if len(stsc) < 8 || len(chunks) == 0 {
		return nil, errors.New("mp4read: no chunk tables")
	}
	type run struct{ first, per int }
	var runs []run
	for i := 0; i < int(u32(stsc[4:])) && 8+12*i+12 <= len(stsc); i++ {
		runs = append(runs, run{int(u32(stsc[8+12*i:])), int(u32(stsc[8+12*i+4:]))})
	}
	si := 0
	for ci := range chunks {
		per := 0
		for _, r := range runs {
			if ci+1 >= r.first {
				per = r.per
			}
		}
		off := chunks[ci]
		for k := 0; k < per && si < count; k++ {
			samples[si].Offset = off
			off += int64(samples[si].Size)
			si++
		}
	}
	if stts := child(stbl, "stts"); len(stts) >= 8 {
		si := 0
		for i := 0; i < int(u32(stts[4:])) && 8+8*i+8 <= len(stts); i++ {
			n, d := int(u32(stts[8+8*i:])), u32(stts[8+8*i+4:])
			for k := 0; k < n && si < count; k++ {
				samples[si].Dur = d
				si++
			}
		}
	}
	if stss := child(stbl, "stss"); len(stss) >= 8 {
		for i := 0; i < int(u32(stss[4:])) && 8+4*i+4 <= len(stss); i++ {
			if k := int(u32(stss[8+4*i:])) - 1; k >= 0 && k < count {
				samples[k].Key = true
			}
		}
	} else {
		for i := range samples {
			samples[i].Key = true
		}
	}
	return samples, nil
}

// readFragments appends each track's samples from each moof, in file
// order. Each traf names its track; one that is not in traks is skipped.
func readFragments(r io.ReaderAt, size int64, traks []*trak, trex map[uint32][3]uint32) error {
	u32 := binary.BigEndian.Uint32
	for _, tk := range traks {
		// Where the next sample starts, in the track's timescale, to
		// stretch the last sample of a fragment over any gap before the
		// next one's tfdt.
		tk.next = 0
		for _, s := range *tk.samples {
			tk.next += uint64(s.Dur)
		}
	}
	return eachTop(r, size, func(typ string, off, h, n int64) (bool, error) {
		if typ != "moof" {
			return true, nil
		}
		moof := make([]byte, n-h)
		if _, err := r.ReadAt(moof, off+h); err != nil {
			return false, err
		}
		// Without a base offset or default-base-is-moof, a traf's data
		// follows the previous traf's.
		dataEnd := off
		for _, tf := range children(moof) {
			if tf.typ != "traf" {
				continue
			}
			tfhd := child(tf.body, "tfhd")
			if len(tfhd) < 8 {
				return false, errors.New("mp4read: short tfhd")
			}
			flags := u32(tfhd) & 0xffffff
			id := u32(tfhd[4:])
			var tk *trak
			for _, k := range traks {
				if k.id == id {
					tk = k
				}
			}
			p := tfhd[8:]
			base := dataEnd
			if flags&0x20000 != 0 {
				base = off
			}
			d := trex[id]
			dur, sz, fl := d[0], d[1], d[2]
			for _, f := range []struct {
				bit uint32
				n   int
				v   *uint32
			}{{0x1, 8, nil}, {0x2, 4, nil}, {0x8, 4, &dur}, {0x10, 4, &sz}, {0x20, 4, &fl}} {
				if flags&f.bit == 0 {
					continue
				}
				if len(p) < f.n {
					return false, errors.New("mp4read: short tfhd")
				}
				if f.bit == 0x1 {
					base = int64(binary.BigEndian.Uint64(p))
				} else if f.v != nil {
					*f.v = u32(p)
				}
				p = p[f.n:]
			}
			if tfdt := child(tf.body, "tfdt"); tk != nil && len(tfdt) >= 8 {
				var dt uint64
				if tfdt[0] == 1 && len(tfdt) >= 12 {
					dt = binary.BigEndian.Uint64(tfdt[4:])
				} else {
					dt = uint64(u32(tfdt[4:]))
				}
				if !tk.timed {
					// The first fragment says where the track starts.
					*tk.start += int64(dt)
					tk.timed = true
				} else if k := len(*tk.samples) - 1; k >= 0 && dt > tk.next && dt-tk.next < 1<<31 {
					(*tk.samples)[k].Dur += uint32(dt - tk.next)
				}
				tk.next = dt
			}
			cont := base
			for _, tr := range children(tf.body) {
				if tr.typ != "trun" {
					continue
				}
				b := tr.body
				if len(b) < 8 {
					return false, errors.New("mp4read: short trun")
				}
				tflags := u32(b) & 0xffffff
				count := int(u32(b[4:]))
				b = b[8:]
				at := cont
				if tflags&0x1 != 0 {
					if len(b) < 4 {
						return false, errors.New("mp4read: short trun")
					}
					at = base + int64(int32(u32(b)))
					b = b[4:]
				}
				first, hasFirst := uint32(0), tflags&0x4 != 0
				if hasFirst {
					if len(b) < 4 {
						return false, errors.New("mp4read: short trun")
					}
					first = u32(b)
					b = b[4:]
				}
				for i := 0; i < count; i++ {
					s := Sample{Offset: at, Size: sz, Dur: dur}
					sf := fl
					if i == 0 && hasFirst {
						sf = first
					}
					for _, f := range []struct {
						bit uint32
						v   *uint32
					}{{0x100, &s.Dur}, {0x200, &s.Size}, {0x400, &sf}, {0x800, nil}} {
						if tflags&f.bit == 0 {
							continue
						}
						if len(b) < 4 {
							return false, errors.New("mp4read: short trun")
						}
						if f.v != nil {
							*f.v = u32(b)
						}
						b = b[4:]
					}
					at += int64(s.Size)
					if tk == nil {
						continue
					}
					// sample_is_non_sync_sample
					s.Key = sf&0x10000 == 0
					*tk.samples = append(*tk.samples, s)
					tk.next += uint64(s.Dur)
				}
				// A later trun without its own offset carries on from here.
				cont, dataEnd = at, at
			}
		}
		return true, nil
	})
}
