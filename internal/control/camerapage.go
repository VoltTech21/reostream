package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
)

// cameraPage is what camera.html renders: the stream state and video this
// camera already has on the dashboard, what GetSupport and GetAbilities say
// the camera is, the probe table that says what it actually answers to,
// and then the curated settings a person actually changes. One camera, one
// page, with two aspects: what config.toml says about it lives on
// /cameras (see camform.go's camerasPage), and everything the camera
// itself will say and let a person change lives here.
type cameraPage struct {
	Title  string
	Camera Camera

	// Schedules are the weekly schedule grids this category shows
	// (schedule.go).
	Schedules []schedSection
	// Zone is the detection zone editor on the motion page (zones.go).
	Zone *zoneView
	// Mask is the privacy mask editor (privacy.go).
	Mask *maskView
	// Rules are the smart rules on the smart page (rules.go); RulesRead
	// says the page asked, so an empty list can say why.
	Rules     []ruleSet
	RulesRead bool
	// TrackLimit is auto tracking's horizontal range (tracklimit.go).
	TrackLimit *trackLimitView
	// PrivacyMode is the privacy mode switch (privacymode.go).
	PrivacyMode *privacyModeView

	// Head is the camera shell (camshell.go); Cat is the settings
	// category this page shows, empty on the Live tab.
	Head camHead
	Cat  string

	// Streams feeds the Live tab's stream picker.
	Streams []liveStream
	// Battery is a camera that sleeps; its Live tab wakes it while open.
	Battery bool
	// CopyURLs is the Live tab's Copy URL list.
	CopyURLs []copyURL
	// Dewarp is true for a fisheye, whose Live tab links the browser dewarp.
	Dewarp bool

	// Clips is the Auto-save motion clips section.
	Clips clipsView

	// PTZ is the Live tab's presets and the Pan, tilt, zoom settings.
	PTZ   ptzView
	Track autoTrackView

	// NetPort is the Network services section.
	NetPort netPortView

	// CGIErr is a failure opening the CGI session a category reads over.
	CGIErr string

	// Defaults is the camera's own factory value for a field, keyed by the
	// field's primary XPath, where the camera reported one. See defaults.go.
	Defaults map[string]string

	// Flash is the one-line report a write on this page left behind -- a
	// curated setting or the floodlight -- or nil when this page was
	// simply opened. See flash.go.
	Flash *Flash

	// Group is this camera's line on the dashboard: its streams' live
	// state, and the video tile, joined by name with s.cameraGroups(). A
	// camera cameraGroups has never heard from (Hubs is nil, or nothing
	// has reported a codec) still renders, as a zero-value group whose
	// Tile is "not playable" rather than dropping the section.
	Group cameraGroup
	// StreamURLs is this camera's streams to their recorder URLs, the same
	// map the status page copies from. Keyed by stream name ("main").
	StreamURLs map[string]string

	Support   *baichuan.Support
	Abilities []baichuan.Ability
	Probes    []BlockProbe

	// Err carries a failure that stopped the support/abilities/probe part
	// of this page from being filled in. It is text for a person, not an
	// error value: this is rendered, never inspected, the same discipline
	// CameraReport.Err already follows.
	Err string

	// Groups, Values, Unavailable, ResolvedBlock and SettingsErr are the
	// curated settings section: what a person actually changes, seeded
	// from a second, independent dial from the one that filled in Support
	// and Abilities above. SettingsErr is its own field, not Err, because
	// a failure reading the curated groups must not blank out the
	// support/abilities/probe section this page already filled in, and
	// vice versa: the two dials fail independently and are reported
	// independently.
	Groups        []Group
	Values        map[string]string
	Unavailable   map[string]string
	ResolvedBlock map[string]string
	SettingsErr   string

	// FloodlightOptions, FloodlightCurrent and FloodlightErr seed the
	// floodlight control. It is not a Field in Groups because its write
	// goes over CGI, not a Baichuan block, so it cannot go through
	// curatedField or writeBlock the way every other control here does; it
	// gets its own section in the template and its own route,
	// serveApplyFloodlight.
	// HasFloodlight is the camera's own answer, from the Support block's
	// ledCtrl, to whether there is a light to drive at all. GetWhiteLed
	// cannot be asked this: measured on the fleet, it returns the same
	// document on a camera with a light and one without, which is how a
	// floodlight control came to be drawn on a camera that has none.
	//
	// False means the camera said no, not that nothing was read: a support
	// read that failed leaves this true, because that is unknown rather
	// than absent.
	HasFloodlight bool
	// HasFishEye is the camera's own answer, from the Support block's
	// fishEye field, to whether it has dewarping view modes: 3 on the
	// fisheye here, 0 on the pano, which stitches two lenses instead.
	//
	// False means the camera said no; a support read that failed leaves
	// this false too, because unlike the floodlight this write reboots the
	// camera. Offering a reboot to a camera that may not support the
	// setting is not a trade worth making.
	HasFishEye     bool
	FishEyeOptions []fisheyeOption
	FishEyeCurrent string
	FishEyeErr     string
	// FishEyeConfirm is what the operator is asked first.
	FishEyeConfirm    string
	FloodlightOptions []floodlightOption
	FloodlightCurrent string
	FloodlightErr     string
	// FloodlightConfirm is the same reason Lights and IR's ConfirmReason
	// carries, repeated here because the floodlight form lives outside
	// Groups and so cannot read it off a Group.
	FloodlightConfirm string
	// HasStitch is the Support block's binoCfg: the camera has two lenses
	// and a seam between them. Measured as the exact inverse of fishEye on
	// this fleet, which is the physical fact -- one lens has no seam, two
	// lenses have nothing to dewarp.
	//
	// Treated like the fisheye rather than the floodlight: false when the
	// support read failed, because a stitch control on a single-lens camera
	// offers to move a seam that does not exist.
	HasStitch bool
	Stitch    stitchPage
	StitchErr string

	// Encoding is what the camera records at, read over CGI like the
	// floodlight rather than from a Baichuan block. The block (56) carries
	// a range for the interframe space only, so a form built from it would
	// offer free number boxes for bitrate and frame rate and discover what
	// the camera refuses by being refused; GetEnc answers that properly,
	// per resolution. See encoding.go.
	Encoding encodingPage

	// Siren is the camera's own noise: speaker volume, and whether it makes
	// a sound when it sees something. Read on the same CGI session. Two
	// controls rather than the eleven the message table suggests, because
	// two is what this firmware answers -- see internal/cgi/audio.go.
	Siren sirenPage
}

// SupportedCount, WantsParamsCount, AbsentCount and HungUpCount give
// camera.html the per-class counts docs/control.md's own probe table
// leads with, without asking the template to do arithmetic.
// Shows reports whether the page draws the curated group with this title.
// The Sound section uses it to leave out its own volume and alarm controls
// where the Speaker and Siren on detection groups already draw them, so a
// camera does not show two volume sliders for one speaker.
func (p cameraPage) Shows(title string) bool {
	for _, g := range p.Groups {
		if g.Title == title {
			return true
		}
	}
	return false
}

func (p cameraPage) SupportedCount() int {
	return p.countWhere(func(b BlockProbe) bool { return b.Supported })
}
func (p cameraPage) WantsParamsCount() int {
	return p.countWhere(func(b BlockProbe) bool { return b.WantsParams })
}
func (p cameraPage) AbsentCount() int {
	return p.countWhere(func(b BlockProbe) bool { return b.Absent })
}
func (p cameraPage) HungUpCount() int {
	return p.countWhere(func(b BlockProbe) bool { return b.HungUp })
}

func (p cameraPage) countWhere(match func(BlockProbe) bool) int {
	n := 0
	for _, b := range p.Probes {
		if match(b) {
			n++
		}
	}
	return n
}

// sliderFloor and sliderCeil are the track a numeric image setting
// (brightness, contrast, saturation) gets by default.
//
// The camera reports its current value for these fields and never reports
// their range: no document this project has read from a camera says what
// the minimum or maximum is. 0-255 is what these fields use on the models
// this project has seen, so it is a good guess for the track -- and it is
// only the track. The number box beside the slider is the input that
// actually posts, and it is not clamped to these bounds, so an operator on
// a model with a different scale can still type the value that model
// wants. Guessing a range is acceptable for a convenience; it would not be
// acceptable as a limit on what can be sent.
const (
	sliderFloor = 0
	sliderCeil  = 255
)

// SliderMin and SliderMax give camera.html the track for one numeric field,
// widened when the value the camera is already holding falls outside the
// default one. Widening rather than clamping matters: a slider whose track
// cannot reach the current value would render with the handle pinned at an
// end, silently showing a value the camera does not hold, and one nudge
// would post it.
//
// A value that is not a number at all (a field this code guessed wrong
// about, say) leaves the default track alone; the number box still shows
// exactly what the camera sent.
func (p cameraPage) SliderMin(current string) int {
	if v, err := strconv.Atoi(strings.TrimSpace(current)); err == nil && v < sliderFloor {
		return v
	}
	return sliderFloor
}

func (p cameraPage) SliderMax(current string) int {
	if v, err := strconv.Atoi(strings.TrimSpace(current)); err == nil && v > sliderCeil {
		return v
	}
	return sliderCeil
}

// positionOption is one corner as the select draws it: the wording a
// person reads, the "x,y" pair choosing it posts (in Field.Paths order, so
// it lines up with the xpath list the same form carries), and whether the
// camera is holding it right now.
type positionOption struct {
	Label    string
	Value    string
	Selected bool
	// Taken names the other overlay already in this corner (the camera
	// cannot draw two there), so the corner is offered grayed out.
	Taken string
}

// positionView is one overlay-position control, ready to render.
//
// Unknown is empty in the ordinary case. It is set when the camera reports
// a topLeftX/topLeftY pair the corner table has no name for, and then it
// carries that pair verbatim, as the currently selected option. The point
// is that an unfamiliar position is SHOWN, never rounded: snapping it to
// the nearest corner would silently throw away a position somebody chose,
// possibly by a route this project knows nothing about, and would do it at
// the moment they opened the page to look. The four corners are still
// offered beneath it, so choosing one remains one click.
type positionView struct {
	Options []positionOption
	Unknown string
	// NotRead is true when the camera gave no value at all, so no corner
	// can honestly be shown as selected.
	NotRead bool
}

// Position is camera.html's view of one Kind "position" field: the four
// corners, with whichever one the camera currently holds selected, or the
// camera's own raw pair when it holds something this project has never
// seen.
func (p cameraPage) Position(f Field) positionView {
	x := p.Values[f.XPath]
	y := p.Values[f.YPath]

	// The name and the timestamp cannot share a corner: the corner the
	// other one is in, while it is shown, is not offered.
	other, otherShown, otherLabel := "OsdDatetime", "OsdDatetime/enable", "the timestamp"
	if strings.HasPrefix(f.XPath, "OsdDatetime/") {
		other, otherShown, otherLabel = "OsdChannelName", "OsdChannelName/enable", "the camera name"
	}
	ox, oy := p.Values[other+"/topLeftX"], p.Values[other+"/topLeftY"]

	view := positionView{}
	known := false
	for _, c := range osdCorners() {
		selected := c.X == x && c.Y == y
		known = known || selected
		opt := positionOption{
			Label:    c.Label,
			Value:    c.X + "," + c.Y,
			Selected: selected,
		}
		if !selected && p.Values[otherShown] != "0" && c.X == ox && c.Y == oy {
			opt.Taken = otherLabel
		}
		view.Options = append(view.Options, opt)
	}
	switch {
	case x == "" && y == "":
		// Nothing was read, which is not the same as an unknown corner:
		// the page says so rather than inventing a "," position.
		view.NotRead = true
	case !known:
		view.Unknown = x + "," + y
	}
	return view
}

// cameraGroupFor finds the dashboard's cameraGroup for one camera by name,
// so a single-camera page can show the same stream state and video tile
// the dashboard already computes, without a second Rows/Tile join.
func (s *Server) cameraGroupFor(name string) cameraGroup {
	for _, g := range s.cameraGroups() {
		if g.Camera == name {
			return g
		}
	}
	return cameraGroup{Camera: name}
}

// liveStream is one stream on the Live tab's picker. A stream this browser
// cannot decode (HEVC) is listed struck through, with the reason, because
// the recorder still gets it.
type liveStream struct {
	Name     string
	Codec    string
	URL      string // same-origin player URL
	Playable bool
	FPS      float64
	Mbps     string
	// Rotate is how far the player turns the picture, degrees
	// anticlockwise: what the stream marks itself with (hub.SetRotate).
	Rotate int
}

// catGroups maps a settings category to the curated groups it shows.
var catGroups = map[string][]string{
	"image":   {"Image"},
	"overlay": {"Name and timestamp"},
	"lights":  {"Lights", "Spotlight"},
	"ftp":     {"FTP upload", "FTP server"},
	"email":   {"Email alerts", "Email server"},
	"push":    {"Push notifications", "Push timing"},
	"detect":  {"Motion detection", "Person detection", "Vehicle detection", "Animal detection"},
	"record":  {"Recording", "Recording length", "Record audio"},
	"audio":   {"Speaker", "Noise reduction", "Siren on detection"},
	"ptz":     {"Tracking method"},
	"network": {"Remote access", "UPnP", "Firmware updates", "Login lockout"},
}

// serveCamera is the Live tab: the player, the stream picker and the
// quick actions. It reads nothing from the camera beyond the shell's
// (cached) identity; the video comes from the hub reostream already holds.
func (s *Server) serveCamera(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	page := cameraPage{
		Title:      cam.Name,
		Camera:     cam,
		Flash:      s.takeFlash(r),
		Group:      s.cameraGroupFor(cam.Name),
		StreamURLs: s.streamURLsFor(r, cam),
	}
	page.Head = s.camHeadFor(r.Context(), cam, "live", "")
	page.CopyURLs = s.copyURLsFor(r, cam)
	page.Support = s.identFor(r.Context(), cam).Support
	page.Dewarp = page.Support != nil && page.Support.HasFishEye(0)
	// The floodlight quick action, only on a camera that says it has one.
	if page.Support != nil && page.Support.HasFloodlight(0) {
		if c, err := s.cgiDial(cam); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), readTimeout*2)
			if mode, state, err := readFloodlight(ctx, c, cam.Channel); err == nil {
				page.HasFloodlight = true
				page.FloodlightCurrent = floodlightState(mode, state)
			}
			cancel()
			c.Close()
		}
	}
	if page.Support != nil && page.Support.HasPTZ() {
		ctx, cancel := context.WithTimeout(r.Context(), readTimeout*2)
		page.PTZ = s.readPresets(ctx, cam)
		cancel()
	}

	codecs := s.codecsByCamera()[cam.Name]
	suffix := map[string]string{"main": "_main", "sub": "_sub", "extern": "_extern", "zoom": "_zoom", "zoom_sub": "_zoom_sub"}
	page.Battery = s.onDemand(cam.Name)
	base := s.opts.StreamBase
	if page.Battery {
		// The public listener refuses a battery camera's stream.
		base = ""
	}
	for _, row := range page.Group.Rows {
		name := streamOf(row.Name)
		codec := codecs[name]
		if codec == "" && page.Battery {
			// Asleep since reostream started, so no keyframe to read the
			// codec from. What battery cameras send: H.264 sub, H.265 main
			// (read off an Argus PT Ultra).
			codec = map[string]string{"sub": "H264", "main": "H265"}[name]
		}
		// H.265 is offered too: whether this browser decodes it is decided
		// in the browser (live.js), not guessed here.
		ls := liveStream{Name: name, Codec: codec, Playable: browserPlayable(codec) || isHEVC(codec),
			FPS: row.FPS, Mbps: row.MbpsBitrate()}
		if s.opts.Hubs != nil {
			if h, ok := s.opts.Hubs.Hub(cam.Name + "/" + name); ok {
				ls.Rotate = h.Rotate()
			}
		}
		if sfx, ok := suffix[name]; ok {
			ls.URL = tileURL(base, "/"+cam.Name+sfx+".ts")
			if name == "main" {
				ls.URL = tileURL(base, "/"+cam.Name+".ts")
			}
		}
		page.Streams = append(page.Streams, ls)
	}
	// The default stream is picked in the browser (live.js): the sharpest
	// one it can decode, or the one last chosen for this camera.
	s.render(w, r, "camera.html", page)
}

// copyURL is one line of the Live tab's Copy URL list.
type copyURL struct{ Label, URL string }

// copyURLsFor lists every address a recorder can read this camera from,
// HTTP then RTSP, per stream in main/sub/extern order, each once.
func (s *Server) copyURLsFor(r *http.Request, cam Camera) []copyURL {
	cfg, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		return nil
	}
	u := recorderURLs(cfg, pageHost(r))
	var out []copyURL
	seen := map[string]bool{}
	add := func(label, url string) {
		if url != "" && !seen[url] {
			seen[url] = true
			out = append(out, copyURL{label, url})
		}
	}
	for _, st := range []string{"main", "sub", "extern"} {
		add(st+" · HTTP", u.StreamHTTP[cam.Name][st])
	}
	for _, st := range []string{"main", "sub", "extern"} {
		add(st+" · RTSP", u.StreamRTSP[cam.Name][st])
	}
	return out
}

// streamURLsFor is this camera's streams to their recorder URLs, built from
// the host the page was reached on so the URL shown is one that can be
// pasted. A config that will not load leaves them empty.
func (s *Server) streamURLsFor(r *http.Request, cam Camera) map[string]string {
	cfg, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		return nil
	}
	return recorderURLs(cfg, pageHost(r)).StreamHTTP[cam.Name]
}

// serveCameraSettingsIndex sends the bare Settings tab to its first
// category.
func (s *Server) serveCameraSettingsIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/cameras/"+url.PathEscape(r.PathValue("name"))+"/settings/image", http.StatusSeeOther)
}

// serveCameraSettings is one category of the Settings tab. Only what that
// category shows is read from the camera: one Baichuan block for a curated
// group, or one CGI session for the controls that live there.
func (s *Server) serveCameraSettings(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cat := r.PathValue("cat")
	page := cameraPage{
		Title:             cam.Name,
		Camera:            cam,
		Flash:             s.takeFlash(r),
		Group:             s.cameraGroupFor(cam.Name),
		StreamURLs:        s.streamURLsFor(r, cam),
		Values:            map[string]string{},
		Unavailable:       map[string]string{},
		ResolvedBlock:     map[string]string{},
		FishEyeOptions:    fisheyeOptions,
		FishEyeConfirm:    fisheyeConfirmReason,
		FloodlightOptions: floodlightOptions,
		FloodlightConfirm: lightsConfirmReason,
	}
	page.Head = s.camHeadFor(r.Context(), cam, "settings", cat)
	if page.Head.CatLabel == "" {
		http.NotFound(w, r)
		return
	}
	page.Title = cam.Name + " · " + page.Head.CatLabel
	page.Cat = cat
	page.Support = s.identFor(r.Context(), cam).Support

	for _, g := range groups() {
		if slices.Contains(catGroups[cat], g.Title) {
			page.Groups = append(page.Groups, g)
		}
	}

	if _, sched := schedBlocks[cat]; len(page.Groups) > 0 || sched || cat == "detect" || cat == "privacy" || cat == "smart" {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if conn, err := s.dial(ctx, cam); err != nil {
			page.SettingsErr = fmt.Sprintf("could not connect: %v", err)
		} else {
			defer conn.Close()
			s.readGroups(ctx, conn, &page)
			s.readSchedule(ctx, conn, &page, cat)
			if cat == "detect" {
				s.readZone(ctx, conn, &page)
			}
			if cat == "privacy" {
				s.readPrivacyMode(ctx, conn, cam.Name, &page)
				s.readMask(ctx, conn, &page)
			}
			if cat == "ptz" {
				s.readTrackLimit(ctx, conn, &page)
			}
			if cat == "smart" {
				page.Rules, page.RulesRead = s.readRules(ctx, conn), true
			}
		}
	}

	if cat == "clips" {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		page.Clips = s.readClipsView(ctx, cam, pageHost(r))
		cancel()
	}
	if cat == "ptz" {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		page.PTZ = s.readGuard(ctx, cam)
		cancel()
		ctx, cancel = context.WithTimeout(r.Context(), probeTimeout)
		page.Track = s.readAutoTrackView(ctx, cam)
		cancel()
	}
	if cat == "network" {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		page.NetPort = s.readNetPort(ctx, cam)
		cancel()
	}

	// The CGI-side controls, each only on the category that shows it.
	needCGI := map[string]bool{"streams": true, "view": true, "stitch": true, "lights": true, "audio": true}[cat]
	if needCGI {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		c, cgiErr := s.cgiDial(cam)
		if cgiErr != nil {
			page.CGIErr = fmt.Sprintf("could not connect: %v", cgiErr)
		} else {
			defer c.Close()
			switch cat {
			case "streams":
				if enc, encErr := readEncoding(ctx, c, cam.Channel); encErr != nil {
					page.Encoding.Err = fmt.Sprintf("could not read the encoding: %v", encErr)
				} else {
					page.Encoding = enc
				}
			case "view":
				page.HasFishEye = page.Support != nil && page.Support.HasFishEye(0)
				if page.HasFishEye {
					if fe, err := readFisheye(ctx, c, cam.Channel); err != nil {
						page.FishEyeErr = fmt.Sprintf("could not read the view mode: %v", err)
					} else {
						page.FishEyeCurrent = fisheyeValueByMode(int(fe.ImageType))
					}
				}
			case "stitch":
				page.HasStitch = page.Support != nil && page.Support.HasStitch(0)
				if page.HasStitch {
					if st, err := readStitch(ctx, c, cam.Channel); err != nil {
						page.StitchErr = fmt.Sprintf("could not read the lens alignment: %v", err)
					} else {
						page.Stitch = st
					}
				}
			case "lights":
				// Hidden only on a positive no; see HasFloodlight.
				page.HasFloodlight = page.Support == nil || page.Support.HasFloodlight(0)
				if page.HasFloodlight {
					if mode, state, readErr := readFloodlight(ctx, c, cam.Channel); readErr != nil {
						page.FloodlightErr = fmt.Sprintf("could not read the floodlight: %v", readErr)
					} else {
						page.FloodlightCurrent = floodlightState(mode, state)
					}
				}
			case "audio":
				page.Siren = readSiren(ctx, c, cam.Channel)
				if enc, err := readEncoding(ctx, c, cam.Channel); err == nil {
					page.Encoding = enc
				}
			}
			if len(page.Groups) > 0 {
				page.Defaults = readDefaults(ctx, c, cam.Channel)
			}
		}
	} else if len(page.Groups) > 0 {
		// The factory values for the reset buttons come over CGI too.
		if c, err := s.cgiDial(cam); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
			page.Defaults = readDefaults(ctx, c, cam.Channel)
			cancel()
			c.Close()
		}
	}

	s.render(w, r, "camera.html", page)
}

// readGroups seeds every curated group on the page from the camera, the
// way the single long camera page used to: a field whose block or element
// is missing is recorded as unavailable, with the reason, rather than
// drawn as a blank control.
func (s *Server) readGroups(ctx context.Context, conn *baichuan.Conn, page *cameraPage) {
	var shown []Group
	defer func() { page.Groups = shown }()
	for _, g := range page.Groups {
		if g.NeedsFloodlight && page.Support != nil && !page.Support.HasFloodlight(0) {
			continue
		}
		if g.NeedsFTP && page.Support != nil && page.Support.LacksFTP() {
			continue
		}
		pair, doc, failReason := resolveGroupBlock(ctx, conn, g)
		if failReason != "" && g.Optional {
			continue
		}
		shown = append(shown, g)
		if failReason != "" {
			for _, f := range g.Fields {
				if f.Kind != "warning" {
					page.Unavailable[f.Key(f.XPath)] = failReason
				}
			}
			continue
		}
		page.ResolvedBlock[g.Title] = pair.Name
		for _, f := range g.Fields {
			if f.Kind == "warning" {
				continue
			}
			// Every element the field edits: a position control is only
			// seedable, and writable, when BOTH of its elements resolved.
			for _, path := range f.Paths() {
				if v, ok := fieldValue(doc, path); ok {
					page.Values[f.Key(path)] = f.shown(v)
					continue
				}
				reason := fmt.Sprintf("%s does not appear in %s on this camera", path, pair.Name)
				page.Unavailable[f.Key(path)] = reason
				page.Unavailable[f.Key(f.XPath)] = reason
			}
		}
	}
}

// choiceView is a Kind "choice" field's stops, with the camera's own value
// selected, and added as its own stop when it is not one of Choices.
type choiceView struct {
	Options []positionOption
	NotRead bool
}

func (p cameraPage) Choice(f Field) choiceView {
	current := p.Values[f.Key(f.XPath)]
	view := choiceView{NotRead: current == ""}
	known := false
	for _, c := range f.Choices {
		selected := c.Value == current
		known = known || selected
		view.Options = append(view.Options, positionOption{Label: c.Label, Value: c.Value, Selected: selected})
	}
	if !known && current != "" {
		view.Options = append(view.Options, positionOption{Label: current, Value: current, Selected: true})
	}
	return view
}

// Reset is the factory value to offer a reset to, or "" when there is
// nothing to offer: the camera reported no default for this field, or the
// field already holds it.
func (p cameraPage) Reset(f Field) string {
	def, ok := p.Defaults[f.XPath]
	if !ok {
		return ""
	}
	current := p.Values[f.Key(f.XPath)]
	if f.YPath != "" {
		current += "," + p.Values[f.YPath]
	}
	if current == def {
		return ""
	}
	return def
}

// ResetLabel is how a default reads on its button: a corner's name rather
// than its two raw numbers, on or off rather than 1 or 0.
func (p cameraPage) ResetLabel(f Field, def string) string {
	switch f.Kind {
	case "position":
		for _, c := range osdCorners() {
			if c.X+","+c.Y == def {
				return c.Label
			}
		}
	case "toggle":
		if def == "1" {
			return "on"
		}
		return "off"
	}
	return def
}

// isHEVC reports an H.265 stream, which some browsers decode and some do not.
func isHEVC(codec string) bool {
	return strings.EqualFold(codec, "H265") || strings.EqualFold(codec, "HEVC")
}
