package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// cameraPage is what camera.html renders: what GetSupport and GetAbilities
// say the camera is, and the probe table that says what it actually
// answers to.
type cameraPage struct {
	Title  string
	Camera Camera

	Support   *baichuan.Support
	Abilities []baichuan.Ability
	Probes    []BlockProbe

	// Err carries a failure that stopped part of this page from being
	// filled in. It is text for a person, not an error value: this is
	// rendered, never inspected, the same discipline internal/control's
	// CameraReport.Err already follows.
	Err string
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

// serveCamera shows one camera: its own account of its hardware and
// permissions, and the probe of every config message this program knows,
// which is the only honest answer to what a model implements.
func (s *CameraServer) serveCamera(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := cameraPage{Title: cam.Name, Camera: cam}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		page.Err = fmt.Sprintf("could not connect: %v", err)
		s.render(w, "camera.html", page)
		return
	}
	// Deferred rather than closed inline after the two reads below: an
	// early return added between the dial and a later close is exactly how
	// the connection-leak bug elsewhere in this package happened, and conn
	// is never reassigned here the way writeBlock's and probeCamera's own
	// conn are, so nothing here needs the closure-over-a-variable pattern
	// those two use.
	defer conn.Close()

	if sup, err := baichuan.GetSupport(ctx, conn); err == nil {
		page.Support = &sup
	} else {
		page.Err = fmt.Sprintf("support read failed: %v", err)
	}
	if ab, err := baichuan.GetAbilities(ctx, conn); err == nil {
		page.Abilities = ab
	} else if page.Err == "" {
		page.Err = fmt.Sprintf("abilities read failed: %v", err)
	}

	probes, err := s.probeCamera(ctx, cam)
	page.Probes = probes
	if err != nil && page.Err == "" {
		page.Err = err.Error()
	}

	s.render(w, "camera.html", page)
}
