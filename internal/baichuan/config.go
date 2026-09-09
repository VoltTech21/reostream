package baichuan

import "sort"

// ConfigMessages names the read requests that take nothing but a channel.
//
// The ids come from the Wireshark dissector's message table. A camera that
// does not implement one answers with status 405 rather than failing the
// connection, so asking for something a model does not have is safe and is
// how this list gets checked against new hardware.
var ConfigMessages = map[string]uint32{
	"alarmevents":   33,
	"audiotask":     232,
	"battery":       252,
	"compression":   56,
	"email":         42,
	"emailtask":     217,
	"floodlight":    291,
	"hddinfo":       102,
	"ip":            76,
	"ledstate":      208,
	"linktype":      93,
	"osdname":       44,
	"ptzserial":     79,
	"ptzzoomfocus":  294,
	"pushinfo":      124,
	"pushtask":      219,
	"recordcfg":     54,
	"recordsched":   81,
	"rfalarm":       133,
	"shelter":       52,
	"streaminfo":    146,
	"support":       199,
	"systemgeneral": 104,
	"timecfg":       287,
	"userlist":      59,
	"version":       80,
	"videoinput":    26,
	"wifi":          116,
	"wifisignal":    115,
}

// ConfigNames lists the keys of ConfigMessages in sorted order.
func ConfigNames() []string {
	out := make([]string, 0, len(ConfigMessages))
	for k := range ConfigMessages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
