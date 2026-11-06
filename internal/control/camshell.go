package control

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// The camera shell is what every page about one camera shares, per the
// redesign: a header with the camera switcher (dropdown, previous / next,
// and the [ ] keys), its state, model, address and firmware, the Live and
// Settings tabs, and on the Settings tab the category list down the left.
// The categories and their order are the design's CATMETA; one is listed
// only when this camera has it and reostream has a working page for it.

type camNavItem struct {
	ID, Label, Href string
	Current         bool
}

type camNavGroup struct {
	Title string
	Items []camNavItem
}

// camHead is the shell's data. Every camera page carries one as Head.
type camHead struct {
	Name     string
	Address  string
	Tab      string // "live" or "settings"
	Cat      string
	Prev     string
	Next     string
	Cameras  []railEntry
	State    string
	Model    string
	Firmware string
	// WiFi is a Wi-Fi camera's signal in dBm (msg 115), 0 when unknown.
	WiFi int
	// Battery is a battery camera's charge in percent (msg 253), when
	// HasBattery says it was read; Charging is whether it is taking a
	// charge right now.
	Battery    int
	HasBattery bool
	Charging   bool
	// DeviceName is the camera's own name, what the Reolink app shows.
	DeviceName string
	// HasSD is a card in the camera; HasClips shows the Recordings tab
	// (a card, or clips the camera saved here).
	HasSD    bool
	HasClips bool
	Nav      []camNavGroup
	// CatLabel is the current category's name, for the page title.
	CatLabel string
}

// camIdent is what the header says about a camera that only the camera
// knows. It changes when firmware does, so it is kept for a while rather
// than read again on every page.
type camIdent struct {
	Support *baichuan.Support
	// HasSD is the camera reporting a storage card (its HddInfoList is not
	// empty).
	HasSD    bool
	Model    string
	Firmware string
	// WiFi is a Wi-Fi camera's signal in dBm (msg 115), 0 when unknown.
	WiFi int
	// Battery is a battery camera's charge in percent (msg 253), when
	// HasBattery says it was read; Charging is whether it is taking a
	// charge right now.
	Battery    int
	HasBattery bool
	Charging   bool
	// DeviceName is the camera's own name, what the Reolink app shows.
	DeviceName string
	// Zoom is a second lens on channel 1 (TrackMix), as the camera's
	// login reply reports it.
	Zoom bool
	at   time.Time
}

const identTTL = 10 * time.Minute

// skipIdent turns the header's own camera read off. Tests set it: their
// fake cameras answer a scripted sequence on one connection, and an extra
// dial for the header would take a reply meant for the page under test.
var skipIdent bool

var (
	identMu    sync.Mutex
	identCache = map[string]camIdent{}
)

// identFor returns the camera's support block, marketing model and
// firmware, from memory when it was read recently. A camera that cannot be
// reached gives a zero ident (nothing cached), and the shell shows less.
func (s *Server) identFor(ctx context.Context, cam Camera) camIdent {
	if skipIdent {
		return camIdent{}
	}
	key := cam.Name + "@" + cam.Address
	identMu.Lock()
	id, ok := identCache[key]
	identMu.Unlock()
	if ok && time.Since(id.at) < identTTL {
		return id
	}
	// A battery camera spends part of this waking up.
	wait := 8 * time.Second
	if cam.UID != "" {
		wait = pageDialTimeoutBattery
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	conn, err := s.dial(ctx, cam)
	if err != nil {
		return camIdent{}
	}
	defer conn.Close()
	id = camIdent{at: time.Now()}
	if sup, err := baichuan.GetSupport(ctx, conn); err == nil {
		id.Support = &sup
	}
	// versioninfo (80) carries the marketing model ("Reolink TrackMix PoE")
	// and the firmware version in one read. DeviceInfo's firmVersion is the
	// serial number on the TrackMix, so it is not used.
	if msg, ok := baichuan.ConfigMessages["versioninfo"]; ok {
		rctx, rcancel := context.WithTimeout(ctx, readTimeout)
		doc, status, err := baichuan.ReadConfig(rctx, conn, msg)
		rcancel()
		if err == nil && status == 200 {
			id.Model, _ = fieldValue(doc, "VersionInfo/type")
			id.Firmware, _ = fieldValue(doc, "VersionInfo/firmwareVersion")
			if !isRecorderConn(conn) {
				if v, ok := fieldValue(doc, "VersionInfo/name"); ok {
					id.DeviceName = strings.TrimSpace(v)
				}
			}
		}
	}
	// Behind a hub or NVR, versioninfo describes the recorder. The
	// recorder's own HTTP API knows the camera on each channel
	// (GetChnTypeInfo: "Reolink TrackMix PoE", its firmware), so ask it.
	if cam.Channel != 0 || isRecorderConn(conn) {
		if c, err := s.cgiDial(cam); err == nil {
			defer c.Close()
			if value, _, _, err := c.Get(ctx, "GetChnTypeInfo", cam.Channel); err == nil {
				var v struct{ TypeInfo, FirmVer string }
				if json.Unmarshal(value, &v) == nil && v.TypeInfo != "" {
					id.Model, id.Firmware = v.TypeInfo, v.FirmVer
				}
			}
		}
	}
	// A card in the slot: the support block's record (3 on an Argus with a
	// card, 0 on a TrackMix without one) or diskNum, which reads 0 for a
	// moment after a battery camera wakes. The sdcard block is no help: a
	// battery camera does not answer it at all.
	// A Wi-Fi camera's signal (msg 115, "<signal>-34</signal>"): weak
	// Wi-Fi is the usual cause of a Wi-Fi camera's broken frames (#17).
	// A battery camera is on Wi-Fi too, though the Argus PT Ultra does
	// not say so in its support block.
	if cam.UID != "" || (id.Support != nil && id.Support.Support.WiFi != 0) {
		// A battery camera that has just woken takes a few seconds to
		// measure its signal.
		wifiWait := readTimeout
		if cam.UID != "" {
			wifiWait = 12 * time.Second
		}
		rctx, rcancel := context.WithTimeout(ctx, wifiWait)
		doc, status, err := baichuan.ReadConfig(rctx, conn, baichuan.MsgIDWifiSignal)
		rcancel()
		if err == nil && status == 200 {
			if v, ok := fieldValue(doc, "WifiSignal/signal"); ok {
				id.WiFi, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
	}
	// A battery camera's own battery report, on the connection this page
	// already woke it for: its charge, and whether it is charging, which
	// is how a charger that is not doing anything gets noticed.
	if cam.UID != "" {
		// Just woken, the Argus answers this one 400 (busy) at first and
		// the real report a second or two later, so ask twice.
		var doc []byte
		var status int16
		var err error
		for try := 0; try < 3; try++ {
			if try > 0 {
				time.Sleep(1500 * time.Millisecond)
			}
			rctx, rcancel := context.WithTimeout(ctx, readTimeout)
			doc, status, err = baichuan.ReadConfig(rctx, conn, baichuan.MsgIDBatteryInfoGet)
			rcancel()
			if err != nil || status == 200 {
				break
			}
		}
		if err == nil && status == 200 {
			if v, ok := fieldValue(doc, "BatteryInfo/batteryPercent"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					id.Battery, id.HasBattery = n, true
				}
			}
			if v, ok := fieldValue(doc, "BatteryInfo/chargeStatus"); ok {
				id.Charging = strings.EqualFold(strings.TrimSpace(v), "charging")
			}
		}
	}
	id.HasSD = id.Support != nil && (id.Support.Support.DiskNum > 0 || id.Support.Support.Record > 0)
	// A TrackMix PoE (v3.0.0.5428) with a card in it says diskNum 0 and
	// record 0 in its support block, and lists the card when asked for it
	// (message 102), so a camera that says none is asked.
	if !id.HasSD && cam.Channel == 0 && !isRecorderConn(conn) {
		rctx, rcancel := context.WithTimeout(ctx, readTimeout)
		doc, status, err := baichuan.ReadConfig(rctx, conn, baichuan.ConfigMessages["sdcard"])
		rcancel()
		id.HasSD = err == nil && status == 200 && hasCard(doc)
	}
	// Behind an NVR or Home Hub the recordings are on the recorder's own
	// disks, searched on this camera's channel; the support block the
	// recorder answers for a channel does not count them, so ask the
	// recorder whether it has a disk at all.
	if cam.Channel != 0 || isRecorderConn(conn) {
		id.HasSD = false
		if c, err := s.cgiDial(cam); err == nil {
			defer c.Close()
			if value, _, _, err := c.Get(ctx, "GetHddInfo", 0); err == nil {
				var v struct {
					HddInfo []json.RawMessage `json:"HddInfo"`
				}
				id.HasSD = json.Unmarshal(value, &v) == nil && len(v.HddInfo) > 0
			}
		}
	}
	// From the login reply, not by asking the zoom lens for video: that
	// took one of the camera's two main stream slots on every read, and on
	// a TrackMix whose slots were taken (main and zoom both streaming) it
	// timed out, so the Cameras page offered no zoom streams to turn on.
	id.Zoom = cam.UID == "" && cam.Channel == 0 && secondLens(conn)
	identMu.Lock()
	identCache[key] = id
	identMu.Unlock()
	return id
}

// identCached is what identFor last read for cam, however long ago, or a
// zero ident when it never has. It never talks to the camera.
func identCached(cam Camera) camIdent {
	identMu.Lock()
	defer identMu.Unlock()
	return identCache[cam.Name+"@"+cam.Address]
}

// identForView is the identity a page that is only being looked at uses.
// A battery camera is not woken for it: reading its name, signal and
// battery wakes it, and the wake watch then saw the camera awake a few
// seconds after that read had ended and logged it as motion, sending a
// false motion event for opening its page (cam-10 Argus, 11-03). So a
// battery camera's Live tab shows what was read last; the camera is only
// talked to once someone acts on it (plays it, opens Settings).
func (s *Server) identForView(ctx context.Context, cam Camera) camIdent {
	if cam.UID != "" {
		return identCached(cam)
	}
	return s.identFor(ctx, cam)
}

// settingsCats is the design's CATMETA, cut to what exists. Each entry is
// shown only when its test passes for this camera.
func settingsCats(sup *baichuan.Support) []struct {
	group string
	items [][2]string
} {
	video := [][2]string{{"streams", "Encoding"}}
	if sup != nil && sup.HasFishEye(0) {
		video = append(video, [2]string{"view", "Fisheye view"})
	}
	if sup != nil && sup.HasStitch(0) {
		video = append(video, [2]string{"stitch", "Lens stitching"})
	}
	hardware := [][2]string{{"lights", "Lights"}, {"audio", "Audio and talk"}}
	if sup != nil && sup.HasPTZ() {
		hardware = append(hardware, [2]string{"ptz", "Pan, tilt, zoom"})
	}
	return []struct {
		group string
		items [][2]string
	}{
		{"Picture", [][2]string{{"image", "Image"}, {"overlay", "Name and timestamp"}, {"privacy", "Privacy mask"}}},
		{"Video", video},
		{"Hardware", hardware},
		{"Detection", [][2]string{{"detect", "Motion and objects"}, {"smart", "Smart rules"}, {"record", "Recording"}}},
		{"Alerts", [][2]string{{"clips", "Auto-save motion clips"}, {"ftp", "FTP upload"}, {"email", "Email alerts"}, {"push", "Push notifications"}}},
		{"System", [][2]string{{"network", "Network services"}, {"time", "Date and time"}, {"remove", "Remove camera"}}},
		{"Expert", [][2]string{{"raw", "Raw values"}, {"advanced", "Raw documents"}}},
	}
}

// catHref is where a category lives. Most are sections of the settings
// tab; time, accounts and the raw documents keep their own pages and
// handlers, drawn inside the same shell.
func catHref(cam, id string) string {
	base := "/cameras/" + cam
	switch id {
	case "time", "accounts", "advanced", "raw":
		return base + "/" + id
	}
	return base + "/settings/" + id
}

// camHeadFor builds the shell for one camera page.
func (s *Server) camHeadFor(ctx context.Context, cam Camera, tab, cat string) camHead {
	h := camHead{Name: cam.Name, Address: cam.Address, Tab: tab, Cat: cat}
	if cams, err := s.fleet(); err == nil {
		h.Cameras = railEntries(cams, s.opts.Status)
		for i, c := range h.Cameras {
			if c.Name != cam.Name {
				continue
			}
			h.State = c.State
			if len(h.Cameras) > 1 {
				h.Prev = h.Cameras[(i+len(h.Cameras)-1)%len(h.Cameras)].Name
				h.Next = h.Cameras[(i+1)%len(h.Cameras)].Name
			}
		}
	}
	var id camIdent
	if tab == "live" {
		id = s.identForView(ctx, cam)
	} else {
		id = s.identFor(ctx, cam)
	}
	// A battery camera records to its card by design, and right after it
	// wakes its support block can read as having none.
	h.Model, h.Firmware, h.HasSD = id.Model, id.Firmware, id.HasSD || cam.UID != ""
	h.WiFi = id.WiFi
	h.Battery, h.HasBattery, h.Charging = id.Battery, id.HasBattery, id.Charging
	h.DeviceName = id.DeviceName
	h.HasClips = h.HasSD || s.hasSavedClips(cam.Name)
	for _, g := range settingsCats(id.Support) {
		ng := camNavGroup{Title: g.group}
		for _, it := range g.items {
			cur := it[0] == cat
			if cur {
				h.CatLabel = it[1]
			}
			ng.Items = append(ng.Items, camNavItem{ID: it[0], Label: it[1], Href: catHref(cam.Name, it[0]), Current: cur})
		}
		h.Nav = append(h.Nav, ng)
	}
	return h
}

// SwitchHref is where the camera switcher sends a camera: the same tab and
// category on the other camera.
func (h camHead) SwitchHref(name string) string {
	switch h.Tab {
	case "live":
		return "/cameras/" + name
	case "recordings":
		return "/cameras/" + name + "/recordings"
	}
	return catHref(name, h.Cat)
}

// hasCard reports whether an HddInfoList (message 102) lists a mounted
// card with some capacity. A camera with no card sends the list empty.
func hasCard(doc []byte) bool {
	var v struct {
		Disks []struct {
			Mount    int   `xml:"mount"`
			Capacity int64 `xml:"capacityV2"`
			CapGB    int64 `xml:"capacity"`
		} `xml:"HddInfoList>HddInfo"`
	}
	if xml.Unmarshal(doc, &v) != nil {
		return false
	}
	for _, d := range v.Disks {
		if d.Mount == 1 && (d.Capacity > 0 || d.CapGB > 0) {
			return true
		}
	}
	return false
}
