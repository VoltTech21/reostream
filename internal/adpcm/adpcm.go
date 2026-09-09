// Package adpcm encodes 16 bit mono PCM as IMA ADPCM, the "DVI4" variant the
// Reolink cameras accept for two-way audio.
//
// The cameras describe what they want in their TalkAbility reply, and every
// camera surveyed here asked for the same thing: adpcm, 16000 Hz, 16 bit
// precision, mono, lengthPerEncoder 1024. lengthPerEncoder is a count of
// samples, so a block is 1024 samples, which is 512 bytes of packed nibbles
// behind a 4 byte preamble carrying the decoder's starting state.
package adpcm

import "encoding/binary"

// SamplesPerBlock is the block length the cameras ask for in
// lengthPerEncoder. A short final block is padded with silence rather than
// sent undersized: the decoder derives its sample count from the block
// length, so a partial block would be decoded as a full one.
const SamplesPerBlock = 1024

// BlockBytes is the encoded size of one block: a 4 byte preamble and one
// nibble per sample.
const BlockBytes = 4 + SamplesPerBlock/2

// stepTable is the IMA ADPCM quantiser ladder, 89 entries.
var stepTable = [89]int32{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230,
	253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
	1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024,
	3327, 3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493,
	10442, 11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623,
	27086, 29794, 32767,
}

// indexTable maps a 4 bit code to the step it moves the ladder by.
var indexTable = [16]int32{-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8}

// Encoder holds the predictor state that carries across blocks. The state is
// also written into each block's preamble, so a decoder can start anywhere,
// but keeping it here means consecutive blocks do not each restart from
// silence and click at the boundary.
type Encoder struct {
	predictor int32
	index     int32
}

func clamp(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// encodeSample folds one sample into the running predictor and returns its
// 4 bit code.
func (e *Encoder) encodeSample(sample int16) byte {
	step := stepTable[e.index]
	delta := int32(sample) - e.predictor

	var code int32
	if delta < 0 {
		code = 8
		delta = -delta
	}
	// The three magnitude bits are a binary search over the step, and the
	// reconstruction below has to mirror this exactly or the encoder and the
	// decoder drift apart within a few samples.
	diff := step >> 3
	if delta >= step {
		code |= 4
		delta -= step
		diff += step
	}
	step >>= 1
	if delta >= step {
		code |= 2
		delta -= step
		diff += step
	}
	step >>= 1
	if delta >= step {
		code |= 1
		diff += step
	}

	if code&8 != 0 {
		e.predictor -= diff
	} else {
		e.predictor += diff
	}
	e.predictor = clamp(e.predictor, -32768, 32767)
	e.index = clamp(e.index+indexTable[code], 0, int32(len(stepTable)-1))
	return byte(code)
}

// EncodeBlock encodes exactly SamplesPerBlock samples into one DVI4 block.
// A short input is padded with silence. The returned slice is BlockBytes long.
//
// The preamble is the predictor and index the decoder must start from, which
// is the state as it stands *before* this block's samples, not after.
func (e *Encoder) EncodeBlock(pcm []int16) []byte {
	out := make([]byte, BlockBytes)
	binary.LittleEndian.PutUint16(out[0:], uint16(int16(e.predictor)))
	out[2] = byte(e.index)
	out[3] = 0

	for i := 0; i < SamplesPerBlock; i++ {
		var s int16
		if i < len(pcm) {
			s = pcm[i]
		}
		code := e.encodeSample(s)
		// Two samples per byte, first sample in the low nibble.
		if i%2 == 0 {
			out[4+i/2] = code
		} else {
			out[4+i/2] |= code << 4
		}
	}
	return out
}

// Decode expands one DVI4 block. It exists so the encoder can be tested
// against a decoder that shares no code with it beyond the two tables.
func Decode(block []byte) []int16 {
	if len(block) < 4 {
		return nil
	}
	predictor := int32(int16(binary.LittleEndian.Uint16(block[0:])))
	index := clamp(int32(block[2]), 0, int32(len(stepTable)-1))

	n := (len(block) - 4) * 2
	out := make([]int16, 0, n)
	for i := 0; i < n; i++ {
		b := block[4+i/2]
		var code int32
		if i%2 == 0 {
			code = int32(b & 0x0F)
		} else {
			code = int32(b >> 4)
		}

		step := stepTable[index]
		diff := step >> 3
		if code&4 != 0 {
			diff += step
		}
		if code&2 != 0 {
			diff += step >> 1
		}
		if code&1 != 0 {
			diff += step >> 2
		}
		if code&8 != 0 {
			predictor -= diff
		} else {
			predictor += diff
		}
		predictor = clamp(predictor, -32768, 32767)
		index = clamp(index+indexTable[code], 0, int32(len(stepTable)-1))
		out = append(out, int16(predictor))
	}
	return out
}
