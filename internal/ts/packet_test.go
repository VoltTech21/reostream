package ts

import "testing"

func TestPacketsAreAlways188BytesAndStartWithSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  []byte
	}{
		{"PAT", writePAT(nil, 0)},
		{"PMT h264", writePMT(nil, 0, StreamTypeH264)},
		{"PMT hevc", writePMT(nil, 0, StreamTypeHEVC)},
	} {
		if len(tc.got) != PacketSize {
			t.Errorf("%s: length %d, want %d", tc.name, len(tc.got), PacketSize)
		}
		if tc.got[0] != 0x47 {
			t.Errorf("%s: first byte %#x, want sync 0x47", tc.name, tc.got[0])
		}
	}
}

func TestPATPointsAtThePMT(t *testing.T) {
	p := writePAT(nil, 0)
	// PID is 13 bits spanning bytes 1 and 2.
	if pid := PID(p[1]&0x1F)<<8 | PID(p[2]); pid != PIDPAT {
		t.Fatalf("PAT carried on PID %#x, want %#x", pid, PIDPAT)
	}
	// A single program whose map PID is PIDPMT must appear in the payload.
	want := []byte{byte(PIDPMT >> 8 & 0x1F), byte(PIDPMT & 0xFF)}
	want[0] |= 0xE0 // upper 3 bits are reserved and set
	if !containsSeq(p, want) {
		t.Fatalf("PAT does not reference PMT PID %#x\n%x", PIDPMT, p)
	}
}

func TestPMTDeclaresTheVideoStreamType(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   byte
	}{
		{"h264", StreamTypeH264},
		{"hevc", StreamTypeHEVC},
	} {
		p := writePMT(nil, 0, tc.st)
		if pid := PID(p[1]&0x1F)<<8 | PID(p[2]); pid != PIDPMT {
			t.Errorf("%s: PMT on PID %#x, want %#x", tc.name, pid, PIDPMT)
		}
		if !containsSeq(p, []byte{tc.st, 0xE0 | byte(PIDVideo>>8), byte(PIDVideo & 0xFF)}) {
			t.Errorf("%s: PMT does not declare stream type %#x on PID %#x\n%x",
				tc.name, tc.st, PIDVideo, p)
		}
	}
}

func TestContinuityCounterWrapsAtFifteen(t *testing.T) {
	// The counter is 4 bits. A decoder uses a discontinuity to decide it has
	// lost packets, so this must wrap cleanly rather than overflow into the
	// adjacent flags.
	seen := make([]byte, 0, 17)
	for cc := 0; cc < 17; cc++ {
		p := writePAT(nil, byte(cc)&0x0F)
		seen = append(seen, p[3]&0x0F)
	}
	for i, got := range seen {
		if want := byte(i) & 0x0F; got != want {
			t.Fatalf("packet %d has continuity %d, want %d", i, got, want)
		}
	}
}

func containsSeq(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
