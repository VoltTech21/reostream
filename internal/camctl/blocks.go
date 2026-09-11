package camctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Confidence is what this codebase knows about a write, not what the camera
// claims. It is a fact about our evidence, recorded in docs/control.md, and
// it should change only when the evidence does: moving a pair up a level
// requires observing an effect on real hardware, never just reading a 200.
type Confidence string

const (
	// Proven: changed, read back on a fresh connection, and restored.
	// docs/control.md names exactly three: osd set, led set, email cfg set.
	Proven Confidence = "proven"
	// Unverified: the write path has the right shape and nobody has
	// observed an effect either way. Most of the 55 pairs are here.
	Unverified Confidence = "unverified"
	// KnownInert: answers 200 with any field changed and re-reads
	// identical, on more than one model. The write is accepted and does
	// nothing. md set is the instructive case: the camera implements the
	// matching read, hands over a full document, accepts it back with any
	// field changed, answers 200, and re-reads unchanged.
	KnownInert Confidence = "known inert"
	// UnsafeToRewrite: the write probably works, and that is the problem.
	// Re-applying an encoder or image configuration makes the camera
	// reconfigure its pipeline, which interrupts the stream. That is a
	// stronger warning than "unverified", so it takes priority over it.
	UnsafeToRewrite Confidence = "unsafe to rewrite"
)

// provenSetIDs and knownInertSetIDs are the only two tables that can move a
// pair off Unverified. Both are drawn from docs/control.md's own record of
// what was actually observed: a field changed, the block read back on a
// fresh connection, and (for the proven three) the original restored.
// Adding an id here means someone watched it happen, not that a write
// returned 200; a 200 is answered by every pair baichuan.ConfigPairs knows
// about, proven and inert alike, and proves nothing on its own.
var provenSetIDs = map[uint32]bool{
	45:  true, // osd set
	209: true, // led set
	43:  true, // email cfg set
}

var knownInertSetIDs = map[uint32]bool{
	47:  true, // md set
	288: true, // floodlight set
}

// confidenceOf labels a read/write pair by what this codebase has actually
// observed about its write, not by what the camera claims when it answers.
// UnsafeToRewrite is checked first because it is the stronger warning: a
// pair that is both unsafe to rewrite and otherwise unverified must read as
// unsafe, not as merely untested.
func confidenceOf(pair baichuan.ConfigPair) Confidence {
	if baichuan.UnsafeToRewrite(pair.Set) {
		return UnsafeToRewrite
	}
	if provenSetIDs[pair.Set] {
		return Proven
	}
	if knownInertSetIDs[pair.Set] {
		return KnownInert
	}
	return Unverified
}

// Block is one readable message as this program actually saw it: the raw
// XML the camera answered with, or the fact that it did not answer 200.
type Block struct {
	Name   string
	ID     uint32
	Status int16
	XML    string
	// HungUp mirrors BlockProbe.HungUp: the camera dropped the connection
	// or stayed silent past readTimeout, which is not the same fact as an
	// answered status and must not be reported as one.
	HungUp bool
}

// PairRow is one writable pair as blocks.html renders it: the label this
// codebase can back up, and the read that seeds its editor. Seed is empty,
// and Editable false, whenever the matching read did not come back as a
// usable 200 document on this probe: the page must never offer an editor
// for a document it did not itself read, because a composed document is
// exactly the thing that breaks models nobody here has tested against.
type PairRow struct {
	Name       string
	Get, Set   uint32
	Confidence Confidence
	Seed       string
	Editable   bool
}

// blocksPage is what blocks.html renders.
type blocksPage struct {
	Title  string
	Camera Camera

	Blocks []Block
	Pairs  []PairRow

	// Err carries a failure that stopped part of this page from being
	// filled in, the same discipline cameraPage.Err already follows: text
	// for a person, never inspected.
	Err string
}

// readBlocks dials cam once and reads every message baichuan.ConfigNames
// knows, recording the XML for each one that answered. It follows
// probeCamera's own redial discipline: a message that gets no answer at all
// is recorded as HungUp and the sweep continues on a fresh connection,
// because the rest of the sweep is still worth having.
func (s *Server) readBlocks(ctx context.Context, cam Camera) ([]Block, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		return nil, fmt.Errorf("camctl: reading blocks for %q: %w", cam.Name, err)
	}
	// See probeCamera's identical comment: conn is reassigned on redial, so
	// this must close whichever connection is current at return time, not
	// the one that existed when the defer statement ran.
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	names := baichuan.ConfigNames()
	out := make([]Block, 0, len(names))
	for _, name := range names {
		id := baichuan.ConfigMessages[name]

		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		xml, status, err := baichuan.ReadConfig(readCtx, conn, id)
		readCancel()

		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
				return out, fmt.Errorf("camctl: reading blocks for %q: %w", cam.Name, ctx.Err())
			}
			out = append(out, Block{Name: name, ID: id, HungUp: true})
			conn.Close()
			conn, err = s.dial(ctx, cam)
			if err != nil {
				return out, fmt.Errorf("camctl: reading blocks for %q: reconnect after %s: %w", cam.Name, name, err)
			}
			continue
		}

		out = append(out, Block{Name: name, ID: id, Status: status, XML: string(xml)})
	}
	return out, nil
}

// buildPairRows labels every pair baichuan.ConfigPairs knows and seeds an
// editor from blocks wherever the matching read came back as a usable 200
// document. A pair whose read is absent, refused, or never answered gets no
// seed and Editable stays false: this is the enforcement of "nothing is
// editable that was not first read from the camera".
func buildPairRows(blocks []Block) []PairRow {
	byID := make(map[uint32]Block, len(blocks))
	for _, b := range blocks {
		byID[b.ID] = b
	}
	pairs := baichuan.ConfigPairs()
	out := make([]PairRow, 0, len(pairs))
	for _, pair := range pairs {
		row := PairRow{
			Name:       pair.Name,
			Get:        pair.Get,
			Set:        pair.Set,
			Confidence: confidenceOf(pair),
		}
		if b, ok := byID[pair.Get]; ok && b.Status == 200 && !b.HungUp {
			row.Seed = b.XML
			row.Editable = true
		}
		out = append(out, row)
	}
	return out
}

// serveBlocks shows every block a camera has: the raw XML for everything
// readable, and for the 55 writable pairs, what this codebase actually
// knows about writing them. The label is about this codebase's evidence,
// not the camera; see docs/control.md for how each proven and known-inert
// entry was established, and confidenceOf's own comment for what it takes
// to move a pair between the tables.
func (s *Server) serveBlocks(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := blocksPage{Title: cam.Name + " blocks", Camera: cam}

	blocks, err := s.readBlocks(r.Context(), cam)
	page.Blocks = blocks
	if err != nil {
		page.Err = err.Error()
	}
	page.Pairs = buildPairRows(blocks)

	s.render(w, "blocks.html", page)
}
