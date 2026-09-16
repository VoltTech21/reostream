// The OSD overlay flags, read on their own so the status page can stay
// free.
//
// Status has always contacted nothing: it reads stream state out of memory
// and renders. The two overlay switches an operator flips most often (the
// timestamp and the camera name) do not live in memory, they live on the
// camera, and reading them is a real Baichuan connection each. Putting that
// read in serveDashboard would have made the page cost one connection per
// camera before a byte of it rendered, which on an eight camera fleet is
// the difference between instant and tens of seconds, and one unreachable
// camera would have held up the whole page.
//
// So the page renders with both switches unknown, and fetches this endpoint
// once per camera afterwards. Each camera's answer arrives, or fails, on its
// own.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// osdFlag is one overlay switch as the camera reports it. Value is the
// camera's own text, unparsed, because that text is what a write posts back
// through setField: On is for drawing the switch, Value is the truth.
//
// Unavailable, when set, says why this camera could not answer for this
// field. It is text for a person: the page shows the switch as unavailable
// with this as its hover title rather than drawing a switch that would lie
// about the camera's state.
type osdFlag struct {
	On          bool   `json:"on"`
	Value       string `json:"value"`
	Unavailable string `json:"unavailable,omitempty"`
}

// osdFlags is the body of GET /cameras/{name}/osd.
//
// Block is the block name that actually answered ("osd get" or "get osd" --
// which one a camera implements is discovered, never assumed; see
// settings.go's Group). The page posts it straight back to
// POST /cameras/{name}/settings, which is still the only write path: this
// endpoint reads, and nothing else.
//
// Fields is keyed by XPath, the same string the settings form posts, so the
// page can match an answer to the control it is filling in without a second
// naming scheme in between.
type osdFlags struct {
	Camera string             `json:"camera"`
	Block  string             `json:"block,omitempty"`
	Fields map[string]osdFlag `json:"fields"`
	Error  string             `json:"error,omitempty"`
}

// osdGroup returns the curated group that owns the overlay switches, so
// this endpoint reads exactly the block and the XPaths the settings page
// already writes, rather than a second list of its own that could drift
// from it.
func osdGroup() (Group, bool) {
	for _, g := range groups() {
		if slices.Contains(g.Blocks, "osd get") {
			return g, true
		}
	}
	return Group{}, false
}

// serveOSD reads one camera's overlay switches.
//
// A camera that cannot be reached, or that does not carry these fields, is
// answered 200 with Error or Unavailable set, not a 5xx. The status page
// fetches this per camera from a script, and the one thing it must be able
// to tell apart is "this camera did not answer" from "this browser is no
// longer logged in": every route here is behind the auth wrapper, which
// answers a stale session with a 303 to /login. Keeping a camera failure on
// the 200 path leaves a non-2xx meaning exactly one thing.
func (s *Server) serveOSD(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	out := osdFlags{Camera: cam.Name, Fields: map[string]osdFlag{}}
	g, ok := osdGroup()
	if !ok {
		out.Error = "no curated block carries the overlay switches"
		writeJSON(w, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		out.Error = fmt.Sprintf("could not connect: %v", err)
		writeJSON(w, out)
		return
	}
	defer conn.Close()

	pair, doc, failReason := resolveGroupBlock(ctx, conn, g)
	if failReason != "" {
		out.Error = failReason
		writeJSON(w, out)
		return
	}
	out.Block = pair.Name

	for _, f := range g.Fields {
		if f.Kind != "toggle" {
			continue
		}
		v, ok := fieldValue(doc, f.XPath)
		if !ok {
			out.Fields[f.XPath] = osdFlag{Unavailable: fmt.Sprintf(
				"%s does not appear in %s on this camera", f.XPath, pair.Name)}
			continue
		}
		// "1" is what the camera writes for on, in every document this
		// codebase has read off one (testdata/livefixtures/osd2.xml).
		// Anything else is reported as off but kept verbatim in Value, so a
		// write sends the camera's own vocabulary back rather than this
		// code's idea of it.
		out.Fields[f.XPath] = osdFlag{On: v == "1", Value: v}
	}
	writeJSON(w, out)
}

// writeJSON writes one small JSON body. Marshalled first, so a failure
// cannot leave a half-written body behind a 200 header.
func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
