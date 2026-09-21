package control

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
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
	FloodlightOptions []floodlightOption
	FloodlightCurrent string
	FloodlightErr     string
	// FloodlightConfirm is the same reason Lights and IR's ConfirmReason
	// carries, repeated here because the floodlight form lives outside
	// Groups and so cannot read it off a Group.
	FloodlightConfirm string
}

// SupportedCount, WantsParamsCount, AbsentCount and HungUpCount give
// camera.html the per-class counts docs/control.md's own probe table
// leads with, without asking the template to do arithmetic.
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
}

// Position is camera.html's view of one Kind "position" field: the four
// corners, with whichever one the camera currently holds selected, or the
// camera's own raw pair when it holds something this project has never
// seen.
func (p cameraPage) Position(f Field) positionView {
	x := p.Values[f.XPath]
	y := p.Values[f.YPath]

	view := positionView{}
	known := false
	for _, c := range osdCorners() {
		selected := c.X == x && c.Y == y
		known = known || selected
		view.Options = append(view.Options, positionOption{
			Label:    c.Label,
			Value:    c.X + "," + c.Y,
			Selected: selected,
		})
	}
	if !known {
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

// serveCamera shows one camera: its stream state and video, its own
// account of its hardware and permissions, the probe of every config
// message this program knows (the only honest answer to what a model
// implements), and then the curated settings a person actually changes.
func (s *Server) serveCamera(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := cameraPage{
		Title:  cam.Name,
		Camera: cam,
		Flash:  s.takeFlash(r),
		Group:  s.cameraGroupFor(cam.Name),

		Groups:            groups(),
		Values:            map[string]string{},
		Unavailable:       map[string]string{},
		ResolvedBlock:     map[string]string{},
		FloodlightOptions: floodlightOptions,
		FloodlightConfirm: lightsConfirmReason,
	}

	// Each phase below gets its own fresh probeTimeout budget rather than
	// sharing one ctx across all four: this page merges four reads that
	// used to be four separate requests (the camera overview, the block
	// probe, the curated settings, the floodlight), each with its own full
	// probeTimeout window. probeCamera alone can spend the whole of a
	// shared budget churning through baichuan.ConfigNames' hundred-odd
	// reads on a camera that answers none of them, which would starve
	// every phase after it of any time at all, not just make it slower.

	supportCtx, supportCancel := context.WithTimeout(r.Context(), probeTimeout)
	defer supportCancel()
	if conn, err := s.dial(supportCtx, cam); err != nil {
		page.Err = fmt.Sprintf("could not connect: %v", err)
	} else {
		// Deferred rather than closed inline after the two reads below: an
		// early return added between the dial and a later close is exactly
		// how the connection-leak bug elsewhere in this package happened,
		// and conn is never reassigned here the way writeBlock's and
		// probeCamera's own conn are, so nothing here needs the
		// closure-over-a-variable pattern those two use.
		defer conn.Close()

		if sup, err := baichuan.GetSupport(supportCtx, conn); err == nil {
			page.Support = &sup
		} else {
			page.Err = fmt.Sprintf("support read failed: %v", err)
		}
		if ab, err := baichuan.GetAbilities(supportCtx, conn); err == nil {
			page.Abilities = ab
		} else if page.Err == "" {
			page.Err = fmt.Sprintf("abilities read failed: %v", err)
		}
	}

	// probeCamera carries its own cameraProbeTimeout (3 minutes) for a
	// sweep of every known message, but this page still bounds the call at
	// probeTimeout, exactly as the standalone camera overview page it
	// replaced did: a camera that answers nothing at all must fail this
	// phase in the same 40 seconds it always has, not eat the full three
	// minutes just because the call no longer has a page of its own.
	probeCtx, probeCancel := context.WithTimeout(r.Context(), probeTimeout)
	defer probeCancel()
	probes, err := s.probeCamera(probeCtx, cam)
	page.Probes = probes
	if err != nil && page.Err == "" {
		page.Err = err.Error()
	}

	// The curated settings read is its own dial, independent of the one
	// above: resolveGroupBlock reads each group's block fresh, and a
	// failure here must not blank out the support/abilities/probe section
	// already filled in.
	settingsCtx, settingsCancel := context.WithTimeout(r.Context(), probeTimeout)
	defer settingsCancel()
	if conn, err := s.dial(settingsCtx, cam); err != nil {
		page.SettingsErr = fmt.Sprintf("could not connect: %v", err)
	} else {
		defer conn.Close()
		for _, g := range page.Groups {
			pair, doc, failReason := resolveGroupBlock(settingsCtx, conn, g)
			if failReason != "" {
				for _, f := range g.Fields {
					if f.Kind != "warning" {
						page.Unavailable[f.XPath] = failReason
					}
				}
				continue
			}
			page.ResolvedBlock[g.Title] = pair.Name
			for _, f := range g.Fields {
				if f.Kind == "warning" {
					continue
				}
				// Every element the field edits, not just its primary one:
				// a position control is only seedable, and only writable,
				// when BOTH of its elements resolved. A missing half is
				// recorded against the field's primary path as well, which
				// is the key the template asks about, so the control is
				// replaced by the explanation rather than rendered half
				// blank.
				for _, path := range f.Paths() {
					if v, ok := fieldValue(doc, path); ok {
						page.Values[path] = v
						continue
					}
					reason := fmt.Sprintf("%s does not appear in %s on this camera", path, pair.Name)
					page.Unavailable[path] = reason
					page.Unavailable[f.XPath] = reason
				}
			}
		}
	}

	// The floodlight is CGI only, an entirely separate transport and
	// session from the Baichuan conns dialed above, so a failure reading
	// it must not blank out either Baichuan section: it is reported on
	// its own, in FloodlightErr.
	floodlightCtx, floodlightCancel := context.WithTimeout(r.Context(), probeTimeout)
	defer floodlightCancel()
	if c, cgiErr := s.cgiDial(cam); cgiErr != nil {
		page.FloodlightErr = fmt.Sprintf("could not connect for the floodlight: %v", cgiErr)
	} else if mode, state, readErr := readFloodlight(floodlightCtx, c); readErr != nil {
		page.FloodlightErr = fmt.Sprintf("could not read the floodlight: %v", readErr)
	} else {
		page.FloodlightCurrent = floodlightState(mode, state)
	}

	s.render(w, "camera.html", page)
}
