package baichuan

import "sort"

// ConfigMessages names the read requests that take nothing but a channel.
//
// The ids and the comments come from the dispatch table in the NVR's
// camera-facing client, not from the Wireshark dissector, which had several
// of them wrong. Three corrections worth naming, because the old values were
// in use: "userlist" was 59, which the firmware calls "Set user cfg" and is a
// write; the battery read is 253, not the 252 report; and 93 is not a
// Baichuan config id at all, it is the ping.
//
// Every id here is one whose firmware description only reads. A camera that
// does not implement one answers status 405 rather than failing the
// connection, so asking a model for something it lacks is safe and is how
// this list gets checked against new hardware.
//
// Generated; see docs/msgids.json.
var ConfigMessages = map[string]uint32{
	"4gmoduleinfo":           257, // get 4g module info
	"accessusercfg":          511, // get access usercfg
	"afalgcfg":               453, // get af alg cfg
	"aicfg":                  299, // ai cfg get
	"aidefaultdetectcfg":     344, // ai default detect cfg get
	"aidenoisecfg":           439, // get ai denoise cfg
	"aidetectcfg":            342, // ai detect cfg get
	"aitracklimitcfg":        434, // get ai track limit cfg
	"aitracktaskcfg":         436, // get ai track task cfg
	"alarmreport":            33,  // alarm report
	"aovinforeport":          687, // aov info report
	"audiocfg":               264, // audio cfg get
	"audiofileinfo":          260, // audio file info get
	"audiofileinfolist":      347, // get audio file info list
	"audiotask":              232, // audio task get
	"autofocuscfg":           224, // get auto focus cfg
	"autoreply":              427, // get auto reply
	"batflagreport":          622, // bat flag report
	"batteryinfo":            253, // battery info get
	"batteryinforeport":      252, // battery info report
	"batterymode":            626, // get battery mode
	"binosttichcfg":          417, // get bino sttich cfg
	"cfgmodifyreport":        580, // cfg modify report
	"coordinatepointreport":  723, // coordinate point report
	"crop":                   228, // crop get
	"crosslinedetectcfg":     527, // get crossline detect cfg
	"delayrecstatreport":     590, // delay rec stat report
	"deviceability":          151, // get device ability
	"dingdongcfg":            486, // get dingdong cfg
	"dingdongscanlistreport": 490, // dingdong scanlist report
	"dns":                    9,   // get dns
	"dst":                    106, // dst get
	"emailcfg":               42,  // email cfg get
	"emailtask":              217, // email task get
	"enc":                    56,  // get enc
	"enc2":                   112, // enc def get
	"encbitrate":             32,  // get encbitrate
	"eventproperties":        21,  // get event properties
	"findfileinfo":           14,  // find file info
	"findfileinfonext":       15,  // find file info next
	"fisheyecfg":             443, // get fish eye cfg
	"floodlightreport":       291, // floodlight report
	"floodlighttask":         289, // floodlight task get
	"fskrfpairreport":        514, // fsk rf pair report
	"ftpcfg":                 68,  // ftp cfg get
	"ftptask":                70,  // ftp task get
	"ftyircutinfo":           371, // fty ir_cut info
	"ftyperipheralstat":      419, // fty peripheral stat get
	"ftyrange":               384, // fty range get
	"ftyrange2":              385, // fty range get
	"ftyresult":              369, // fty result get
	"general":                104, // get general
	"homebasewakeuptask":     488, // get homebase wakeup task
	"intrusiondetectcfg":     529, // get intrusion detect cfg
	"isp":                    26,  // isp get
	"isp2":                   132, // isp def
	"ispbase":                78,  // isp base
	"largebattery":           449, // get large battery
	"led":                    208, // led get
	"legacydetectcfg":        549, // get legacy detect cfg
	"linewidth":              361, // linewidth get
	"locallink":              76,  // get local link
	"loiteringdetectcfg":     531, // get loitering detect cfg
	"longruncfg":             594, // get longrun cfg
	"lossdetectcfg":          551, // get loss detect cfg
	"md":                     46,  // md get
	"md2":                    113, // md def get
	"osd":                    29,  // get osd
	"osd2":                   44,  // osd get
	"osd3":                   110, // osd def get
	"performanceinfo":        122, // performance info
	"pirmotiondetectcfg":     694, // get pir motion detect cfg
	"powermode":              451, // get power mode
	"presets":                8,   // get presets
	"prisign":                729, // get pri sign
	"ptzcurpos":              433, // get ptz cur_pos
	"ptzparam":               79,  // ptz param
	"pushtask":               219, // push task get
	"reccfg":                 54,  // rec cfg get
	"rectask":                81,  // rec task get
	"rfcfg":                  212, // rf cfg get
	"rptstationlist":         563, // rpt station list get
	"sdcard":                 102, // sdcard get
	"shelter":                52,  // shelter get
	"shelter2":               111, // shelter def
	"silentmode":             609, // get silent mode
	"sirenstatusreport":      547, // siren status report
	"support":                199, // get support
	"syscpuload":             31,  // get syscpuload
	"sysdatatime":            6,   // get sys data time
	"sysversion":             34,  // get sysversion
	"talkability":            10,  // talk ability
	"tamperalarmcfg":         763, // get tamper alarm cfg
	"threshold":              296, // threshold get
	"timelapsecfg":           319, // get timelapse cfg
	"uidcfg":                 114, // get uid cfg
	"usercfg":                58,  // get user cfg
	"versioninfo":            80,  // version info
	"wifiinfo":               116, // wifi info get
	"wifiretcode":            553, // get wifi retcode
	"wifisdbinfo":            537, // wifi sdb info get
	"zfbacklash":             365, // zf backlash get
	"zoomfocus":              294, // zoom focus get
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
