package baichuan

import "testing"

// These ids are the ones proven against live cameras, and they are what makes
// the rest of the recovered table trustworthy. If the generator is ever rerun
// against a different firmware and these move, the table is not the one this
// client speaks.
func TestRecoveredIDsMatchWhatCamerasAnswer(t *testing.T) {
	for id, want := range map[uint32]string{
		1:   "login",
		3:   "preview start",
		10:  "talk ability",
		45:  "osd set",
		109: "snap",
		151: "get device ability",
		199: "get support",
		201: "talk open",
		202: "talk fdx stream",
		208: "led get",
		209: "led set",
	} {
		if got := MsgName(id); got != want {
			t.Errorf("MsgName(%d) = %q, want %q", id, got, want)
		}
	}
}

// 5 is the correction that matters most. It was recorded as HeartBeat, which
// came from the NVR's internal IPC enum where index 5 is MSG_APP_HB. That is
// a different namespace, and every camera answered 421 because it was being
// asked to start playback.
func TestFiveIsReplayStartNotHeartBeat(t *testing.T) {
	if got := MsgName(5); got != "replay start" {
		t.Errorf("MsgName(5) = %q, want %q", got, "replay start")
	}
	if MsgName(0) != "heartbeat" {
		t.Errorf("MsgName(0) = %q, want the Baichuan heartbeat", MsgName(0))
	}
}

// An id the table does not carry must read as unknown rather than as some
// neighbouring message, since the table is explicitly not exhaustive.
func TestUnknownIDHasNoName(t *testing.T) {
	if got := MsgName(99999); got != "" {
		t.Errorf("MsgName(99999) = %q, want empty", got)
	}
	// 93 is the ping, which the camera answers but the dispatch table in
	// the NVR's client does not list.
	if got := MsgName(93); got != "" {
		t.Errorf("MsgName(93) = %q, want empty: 93 is not in the table", got)
	}
}

// Every read in ConfigMessages must be a message the firmware describes as a
// read. This is the guard that would have caught "userlist" being 59, which
// the firmware calls "Set user cfg" and which a sweep was sending blind.
func TestConfigMessagesContainsNoWrites(t *testing.T) {
	for name, id := range ConfigMessages {
		desc := MsgName(id)
		if desc == "" {
			t.Errorf("%s (%d) is not in the recovered table", name, id)
			continue
		}
		for _, verb := range []string{"set", "start", "stop", "reboot", "restore", "format", "delete", "import"} {
			if containsWord(desc, verb) {
				t.Errorf("%s (%d) is %q, which acts; it must not be in a read sweep", name, id, desc)
			}
		}
	}
}

// containsWord reports whether desc has verb as a whole space delimited word.
func containsWord(desc, verb string) bool {
	start := 0
	for i := 0; i <= len(desc); i++ {
		if i == len(desc) || desc[i] == ' ' {
			if desc[start:i] == verb {
				return true
			}
			start = i + 1
		}
	}
	return false
}

// Pairing must be on the firmware's exact wording. Normalising "osd get" and
// "get osd" onto one key pairs 44 with 30, and a write sent to the wrong
// message id is the one mistake here with real consequences.
func TestConfigPairsMatchOnExactDescription(t *testing.T) {
	got := make(map[uint32]uint32)
	for _, p := range ConfigPairs() {
		got[p.Get] = p.Set
	}
	for get, set := range map[uint32]uint32{
		44:  45,  // osd get -> osd set
		29:  30,  // get osd -> set osd
		46:  47,  // md get  -> md set
		208: 209, // led get -> led set
		26:  25,  // isp get -> isp set, where the write is the lower id
		217: 216, // email task get -> email task set, likewise
	} {
		if got[get] != set {
			t.Errorf("pair for %d (%q) = %d, want %d", get, MsgName(get), got[get], set)
		}
	}
	// "osd def get" has no "osd def set", so it must not be paired at all.
	if s, ok := got[110]; ok {
		t.Errorf("110 (%q) paired with %d; it has no write", MsgName(110), s)
	}
}

// The guard that keeps a rewrite sweep from interrupting a live camera.
func TestUnsafeToRewriteCoversTheDisruptiveWrites(t *testing.T) {
	for _, id := range []uint32{57, 25, 105, 196} {
		if !UnsafeToRewrite(id) {
			t.Errorf("%d (%q) should be held back from a rewrite sweep", id, MsgName(id))
		}
	}
	for _, id := range []uint32{45, 47, 209} {
		if UnsafeToRewrite(id) {
			t.Errorf("%d (%q) is safe to rewrite and should not be held back", id, MsgName(id))
		}
	}
}
