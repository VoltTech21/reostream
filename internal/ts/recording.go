package ts

import (
	"io"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// RecordingWriter muxes the frames of one downloaded recording into a
// single MPEG-TS stream. The muxer is made from the first video frame's
// codec, since a recording's codec is only known once it arrives; audio
// before that, which has no picture to sit beside, is dropped.
type RecordingWriter struct {
	w     io.Writer
	m     *Muxer
	audio bool
	err   error
}

// NewRecordingWriter writes to w. audio says whether the recording has an
// AAC track to declare.
func NewRecordingWriter(w io.Writer, audio bool) *RecordingWriter {
	return &RecordingWriter{w: w, audio: audio}
}

// Write adds one frame.
func (r *RecordingWriter) Write(f baichuan.Frame) error {
	if r.err != nil {
		return r.err
	}
	if r.m == nil {
		if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
			return nil
		}
		var m *Muxer
		var err error
		codec := recordedCodec(f)
		if r.audio {
			m, err = NewMuxerWithAudio(codec, "aac")
		} else {
			m, err = NewMuxer(codec)
		}
		if err != nil {
			r.err = err
			return err
		}
		r.m = m
		if _, err := r.w.Write(m.Header()); err != nil {
			r.err = err
			return err
		}
	}
	if f.Kind == baichuan.FrameAAC && !r.audio {
		return nil
	}
	if f.Kind == baichuan.FrameADPCM {
		return nil
	}
	if b := r.m.Frame(f); len(b) > 0 {
		if _, err := r.w.Write(b); err != nil {
			r.err = err
			return err
		}
	}
	return nil
}

// recordedCodec is the codec of a recording's first video frame, read from
// the picture itself: a TrackMix WiFi (v3.0.0.5428) labels its recorded
// frames H264 while they hold H.265 (#21). The first NAL unit tells them
// apart: an H.265 one has a two-byte header whose second byte is 1 for
// the base layer, and its parameter sets and IDR types make no sense as
// H.264.
func recordedCodec(f baichuan.Frame) string {
	b := f.Data
	for i := 0; i+4 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 || b[i+2] != 1 {
			continue
		}
		h, next := b[i+3], b[i+4]
		switch (h >> 1) & 0x3f {
		case 32, 33, 34, 35, 39, 19, 20:
			if h&0x81 == 0 && next == 1 {
				return "H265"
			}
		}
		switch h & 0x1f {
		case 5, 6, 7, 8, 9:
			return "H264"
		}
		return f.Codec
	}
	return f.Codec
}
