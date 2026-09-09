package rtsp

// H265 NALU types this package cares about. The parameter sets have to be
// pulled out of the elementary stream because Reolink sends them inline with
// every keyframe rather than out of band, and they belong in the SDP.
const (
	h265VPS = 32
	h265SPS = 33
	h265PPS = 34
)

// H264 NALU types, same reasoning.
const (
	h264SPS = 7
	h264PPS = 8
)

// h265NALUType reads the NAL unit type from an H265 NALU header, which holds
// it in the six bits below the forbidden_zero_bit.
func h265NALUType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return (nalu[0] >> 1) & 0x3f
}

// h264NALUType reads the NAL unit type from an H264 NALU header, which holds
// it in the low five bits.
func h264NALUType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return nalu[0] & 0x1f
}

// splitAnnexB splits an Annex-B elementary stream into its NAL units.
//
// Start code framing is identical for H264 and H265, so this serves both.
// Written here rather than imported from a codec package because importing
// the H264 one to frame H265 reads as a mistake at every later glance.
//
// Zero length units are dropped: a stream ending on a start code would
// otherwise yield one, and an empty NALU becomes a zero length RTP payload
// that a decoder has to guess about.
func splitAnnexB(es []byte) [][]byte {
	var out [][]byte
	start := -1
	i := 0
	for i+3 <= len(es) {
		n := startCodeLen(es[i:])
		if n == 0 {
			i++
			continue
		}
		if start >= 0 {
			if nalu := es[start:i]; len(nalu) > 0 {
				out = append(out, nalu)
			}
		}
		i += n
		start = i
	}
	if start >= 0 && start < len(es) {
		out = append(out, es[start:])
	}
	return out
}

// startCodeLen returns 4 for a 00 00 00 01 start code, 3 for 00 00 01, and 0
// for anything else.
func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}
