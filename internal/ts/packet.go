package ts

import "encoding/binary"

// PacketSize is fixed by the transport stream format.
const PacketSize = 188

type PID uint16

// PID assignment. These are arbitrary within the allowed range and only have
// to agree between the PMT and the packets themselves.
const (
	PIDPAT   PID = 0x0000
	PIDPMT   PID = 0x1000
	PIDVideo PID = 0x0100
	PIDAudio PID = 0x0101
)

// Stream types as registered in the MPEG systems tables.
const (
	StreamTypeH264 byte = 0x1B
	StreamTypeHEVC byte = 0x24
	StreamTypeAAC  byte = 0x0F
)

// writeHeader writes the 4 byte transport packet header into dst[:4].
// payloadStart marks a packet that begins a new PSI section or PES packet.
func writeHeader(dst []byte, pid PID, cc byte, payloadStart bool) {
	dst[0] = 0x47
	dst[1] = byte(pid >> 8 & 0x1F)
	if payloadStart {
		dst[1] |= 0x40
	}
	dst[2] = byte(pid & 0xFF)
	// 0x10 is "payload present, no adaptation field".
	dst[3] = 0x10 | (cc & 0x0F)
}

// writeSection wraps a PSI section in a full transport packet, padding the
// remainder with 0xFF as the format requires.
func writeSection(dst []byte, pid PID, cc byte, section []byte) []byte {
	if cap(dst) < PacketSize {
		dst = make([]byte, PacketSize)
	}
	dst = dst[:PacketSize]
	for i := range dst {
		dst[i] = 0xFF
	}
	writeHeader(dst, pid, cc, true)
	dst[4] = 0x00 // pointer_field: the section starts immediately after
	copy(dst[5:], section)
	return dst
}

func writePAT(dst []byte, cc byte) []byte {
	// One program, number 1, whose map lives on PIDPMT.
	body := []byte{
		0x00, 0x01, // program_number 1
		0xE0 | byte(PIDPMT>>8&0x1F), byte(PIDPMT & 0xFF),
	}
	return writeSection(dst, PIDPAT, cc, psiSection(0x00, 0x0001, body))
}

func writePMT(dst []byte, cc byte, streamType byte) []byte {
	body := []byte{
		0xE0 | byte(PIDVideo>>8&0x1F), byte(PIDVideo & 0xFF), // PCR PID
		0xF0, 0x00, // program_info_length 0
		streamType,
		0xE0 | byte(PIDVideo>>8&0x1F), byte(PIDVideo & 0xFF),
		0xF0, 0x00, // ES_info_length 0
	}
	return writeSection(dst, PIDPMT, cc, psiSection(0x02, 0x0001, body))
}

// psiSection builds a PSI section: table header, body, and the CRC the format
// requires over everything preceding it.
func psiSection(tableID byte, extension uint16, body []byte) []byte {
	// 5 bytes of section header after the length field, then body, then CRC.
	sectionLen := 5 + len(body) + 4
	s := make([]byte, 0, 3+sectionLen)
	s = append(s, tableID)
	// section_syntax_indicator set, reserved bits set, then a 12 bit length.
	s = append(s, 0xB0|byte(sectionLen>>8&0x0F), byte(sectionLen&0xFF))
	s = binary.BigEndian.AppendUint16(s, extension)
	s = append(s, 0xC1) // version 0, current, section 0 of 0
	s = append(s, 0x00, 0x00)
	s = append(s, body...)
	return binary.BigEndian.AppendUint32(s, crc32MPEG(s))
}

// crc32MPEG is the CRC-32/MPEG-2 the PSI tables are defined with. It is not
// the same polynomial arrangement as hash/crc32's IEEE, so it is written out
// rather than reached for from the standard library.
func crc32MPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
