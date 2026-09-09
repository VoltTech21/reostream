package baichuan

import (
	"sort"
	"strings"
)

// ConfigPair is a read message and the write that takes the same document.
type ConfigPair struct {
	Get  uint32
	Set  uint32
	Name string // the read's description, as the firmware words it
}

// ConfigPairs returns the read/write pairs in the recovered table.
//
// Pairing is on the firmware's exact description, "osd get" to "osd set",
// rather than on a normalised key. Normalising collapses "osd get" (44) and
// "get osd" (29) onto one name and pairs 44 with 30, and a write sent to the
// wrong message id is the one mistake in this package with real consequences.
//
// A read with no matching write, such as "osd def get", yields no pair.
func ConfigPairs() []ConfigPair {
	byName := make(map[string]uint32, len(msgNames))
	for id, name := range msgNames {
		// Ids are unique per name here, and where two ids share a name the
		// lower one is the one an NVR sends.
		if prev, ok := byName[name]; !ok || id < prev {
			byName[name] = id
		}
	}
	var out []ConfigPair
	for id, name := range msgNames {
		var want string
		switch {
		case strings.HasSuffix(name, " get"):
			want = strings.TrimSuffix(name, " get") + " set"
		case strings.HasPrefix(name, "get "):
			want = "set " + strings.TrimPrefix(name, "get ")
		default:
			continue
		}
		if setID, ok := byName[want]; ok {
			out = append(out, ConfigPair{Get: id, Set: setID, Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Get < out[j].Get })
	return out
}

// UnsafeToRewrite reports whether writing a document back to this message,
// even unchanged, could disturb a camera that is carrying traffic.
//
// Re-applying an encoder or image configuration makes the camera reconfigure
// the pipeline, which interrupts the stream, and a firmware update or reboot
// setting speaks for itself. Nothing here is about the message being
// malformed; these are the writes whose success is the problem.
func UnsafeToRewrite(id uint32) bool {
	switch msgNames[id] {
	case "set enc", "isp set", "set general", "set auto update",
		"wifi info set", "wifi sdb info set", "set access usercfg",
		"set power mode", "set battery mode", "set sleep state cfg",
		"set longrun cfg", "set fish eye cfg", "set bino sttich cfg",
		"fty peripheral stat set", "set audio file info list":
		return true
	}
	return false
}
