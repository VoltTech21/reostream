package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// packet is one TCP segment's payload with its endpoints.
type packet struct {
	src, dst         string
	srcPort, dstPort uint16
	payload          []byte
}

// readPcap reads a classic pcap file. pcapng is a different container and is
// not handled; Wireshark will save either, so choose "pcap" in the save
// dialog, or convert with: editcap -F pcap in.pcapng out.pcap
func readPcap(path string) ([]packet, error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(d) < 24 {
		return nil, fmt.Errorf("file is too short to be a capture")
	}

	var bo binary.ByteOrder
	switch {
	case string(d[:4]) == "\xd4\xc3\xb2\xa1":
		bo = binary.LittleEndian
	case string(d[:4]) == "\xa1\xb2\xc3\xd4":
		bo = binary.BigEndian
	case string(d[:4]) == "\x0a\x0d\x0d\x0a":
		return nil, fmt.Errorf("this is a pcapng file; save as pcap, or run: editcap -F pcap in.pcapng out.pcap")
	default:
		return nil, fmt.Errorf("not a pcap file (magic %x)", d[:4])
	}
	linkType := bo.Uint32(d[20:])

	var out []packet
	for off := 24; off+16 <= len(d); {
		caplen := int(bo.Uint32(d[off+8:]))
		if off+16+caplen > len(d) {
			break
		}
		frame := d[off+16 : off+16+caplen]
		off += 16 + caplen
		if p, ok := parseFrame(frame, linkType); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// parseFrame pulls the TCP payload out of one captured frame.
func parseFrame(f []byte, linkType uint32) (packet, bool) {
	var l3 []byte
	switch linkType {
	case 1: // Ethernet
		if len(f) < 14 {
			return packet{}, false
		}
		etherType := binary.BigEndian.Uint16(f[12:])
		l3 = f[14:]
		// One VLAN tag, which a capture on a trunk port will carry.
		if etherType == 0x8100 {
			if len(f) < 18 {
				return packet{}, false
			}
			etherType = binary.BigEndian.Uint16(f[16:])
			l3 = f[18:]
		}
		if etherType != 0x0800 {
			return packet{}, false
		}
	case 101, 12: // raw IP
		l3 = f
	case 113: // Linux cooked capture, which "tcpdump -i any" produces
		if len(f) < 16 || binary.BigEndian.Uint16(f[14:]) != 0x0800 {
			return packet{}, false
		}
		l3 = f[16:]
	default:
		return packet{}, false
	}

	if len(l3) < 20 || l3[0]>>4 != 4 {
		return packet{}, false
	}
	ihl := int(l3[0]&0x0f) * 4
	if l3[9] != 6 || len(l3) < ihl+20 {
		return packet{}, false
	}
	total := int(binary.BigEndian.Uint16(l3[2:]))
	if total > len(l3) {
		total = len(l3)
	}
	src := fmt.Sprintf("%d.%d.%d.%d", l3[12], l3[13], l3[14], l3[15])
	dst := fmt.Sprintf("%d.%d.%d.%d", l3[16], l3[17], l3[18], l3[19])

	tcp := l3[ihl:total]
	if len(tcp) < 20 {
		return packet{}, false
	}
	doff := int(tcp[12]>>4) * 4
	if doff > len(tcp) {
		return packet{}, false
	}
	return packet{
		src: src, dst: dst,
		srcPort: binary.BigEndian.Uint16(tcp[0:]),
		dstPort: binary.BigEndian.Uint16(tcp[2:]),
		payload: tcp[doff:],
	}, true
}

// xmlKey is the BC cipher key, the same on every camera.
var xmlKey = [8]byte{0x1f, 0x2d, 0x3c, 0x4b, 0x5a, 0x69, 0x78, 0xff}

var aesIV = []byte("0123456789abcdef")

// decrypt undoes whichever cipher covers a message body: the BC XOR during
// the handshake, AES once the key is known.
func decrypt(body, key []byte, encOffset uint32) []byte {
	if len(body) == 0 {
		return nil
	}
	if key != nil {
		block, err := aes.NewCipher(key)
		if err == nil {
			out := make([]byte, len(body))
			cipher.NewCFBDecrypter(block, aesIV).XORKeyStream(out, body)
			if looksLikeXML(out) {
				return out
			}
		}
	}
	out := make([]byte, len(body))
	off := int(encOffset % 8)
	ob := byte(encOffset)
	for i, c := range body {
		out[i] = c ^ xmlKey[(off+i)%8] ^ ob
	}
	if looksLikeXML(out) {
		return out
	}
	return body
}

func looksLikeXML(b []byte) bool {
	return len(b) > 5 && strings.Contains(string(b[:min(len(b), 64)]), "<?xml")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var nonceRe = regexp.MustCompile(`<nonce>([^<]+)</nonce>`)

func findNonce(x []byte) string {
	m := nonceRe.FindSubmatch(x)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// aesKey is the first 16 characters of the uppercase hex MD5 of
// "nonce-password".
func aesKey(nonce, password string) []byte {
	sum := md5.Sum([]byte(nonce + "-" + password))
	return []byte(strings.ToUpper(fmt.Sprintf("%x", sum))[:16])
}
