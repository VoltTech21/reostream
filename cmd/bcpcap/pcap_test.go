package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildPcap wraps two byte streams in the minimum framing a capture file
// needs, so the reader can be tested against the committed conversation
// fixtures without needing a real capture in the repository.
func buildPcap(t *testing.T, c2s, s2c []byte) string {
	t.Helper()

	var out []byte
	hdr := make([]byte, 24)
	copy(hdr, []byte{0xd4, 0xc3, 0xb2, 0xa1})
	binary.LittleEndian.PutUint16(hdr[4:], 2)
	binary.LittleEndian.PutUint16(hdr[6:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], 65535)
	binary.LittleEndian.PutUint32(hdr[20:], 1) // Ethernet
	out = append(out, hdr...)

	// Split each direction into segments so reassembly is exercised rather
	// than a single packet carrying everything.
	add := func(payload []byte, toCamera bool) {
		for off := 0; off < len(payload); off += 500 {
			end := off + 500
			if end > len(payload) {
				end = len(payload)
			}
			seg := payload[off:end]

			eth := make([]byte, 14)
			binary.BigEndian.PutUint16(eth[12:], 0x0800)

			ip := make([]byte, 20)
			ip[0] = 0x45
			ip[9] = 6
			binary.BigEndian.PutUint16(ip[2:], uint16(20+20+len(seg)))
			client := []byte{192, 0, 2, 10}
			camera := []byte{192, 0, 2, 20}
			if toCamera {
				copy(ip[12:], client)
				copy(ip[16:], camera)
			} else {
				copy(ip[12:], camera)
				copy(ip[16:], client)
			}

			tcp := make([]byte, 20)
			if toCamera {
				binary.BigEndian.PutUint16(tcp[0:], 50000)
				binary.BigEndian.PutUint16(tcp[2:], 9000)
			} else {
				binary.BigEndian.PutUint16(tcp[0:], 9000)
				binary.BigEndian.PutUint16(tcp[2:], 50000)
			}
			tcp[12] = 5 << 4

			frame := append(append(append(eth, ip...), tcp...), seg...)
			rec := make([]byte, 16)
			binary.LittleEndian.PutUint32(rec[8:], uint32(len(frame)))
			binary.LittleEndian.PutUint32(rec[12:], uint32(len(frame)))
			out = append(out, rec...)
			out = append(out, frame...)
		}
	}
	add(c2s, true)
	add(s2c, false)

	path := filepath.Join(t.TempDir(), "test.pcap")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "baichuan", "testdata", name))
	if err != nil {
		t.Skipf("fixture %s not available: %v", name, err)
	}
	return b
}

func TestReadPcapReassemblesBothDirections(t *testing.T) {
	c2s := fixture(t, "login_c2s.bin")
	s2c := fixture(t, "login_s2c.bin")

	packets, err := readPcap(buildPcap(t, c2s, s2c))
	if err != nil {
		t.Fatal(err)
	}

	var toCam, fromCam []byte
	for _, p := range packets {
		if p.dstPort == 9000 {
			toCam = append(toCam, p.payload...)
		} else {
			fromCam = append(fromCam, p.payload...)
		}
	}
	if string(toCam) != string(c2s) {
		t.Errorf("client stream reassembled to %d bytes, want %d", len(toCam), len(c2s))
	}
	if string(fromCam) != string(s2c) {
		t.Errorf("camera stream reassembled to %d bytes, want %d", len(fromCam), len(s2c))
	}
}

// The point of the tool is turning a capture into an id-to-name mapping, so
// the login exchange has to come back as message 1 carrying a nonce.
func TestDecodeRecoversMessageIdsAndTheNonce(t *testing.T) {
	s2c := fixture(t, "login_s2c.bin")
	seen := map[uint32]map[string]int{}
	decodeStream(s2c, "admin", "", false, seen)

	if len(seen) == 0 {
		t.Fatal("no messages decoded from the camera's login reply")
	}
	names, ok := seen[1]
	if !ok {
		t.Fatalf("message 1 not seen; ids found: %v", keys(seen))
	}
	if _, ok := names["Encryption"]; !ok {
		if _, ok := names["DeviceInfo"]; !ok {
			t.Errorf("message 1 carried %v, want the Encryption or DeviceInfo element", names)
		}
	}
}

func keys(m map[uint32]map[string]int) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReadPcapRejectsPcapngWithAdvice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.pcapng")
	if err := os.WriteFile(path, []byte("\x0a\x0d\x0d\x0a0000000000000000000000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readPcap(path)
	if err == nil {
		t.Fatal("want an error for a pcapng file")
	}
	// The message has to say what to do about it, since Wireshark saves
	// pcapng by default and this is the first thing anyone will hit.
	if !contains(err.Error(), "editcap") {
		t.Errorf("error %q does not say how to convert the file", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
