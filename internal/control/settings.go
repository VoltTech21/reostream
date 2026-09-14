// Curated settings: a small number of fields a person actually changes,
// picked out of the raw blocks blocks.html already exposes wholesale, so
// changing the camera's name or its timestamp overlay does not require
// editing an XML document by hand.
package control

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Field is one curated control: a human label, the path to the element it
// edits, and how to present it.
//
// XPath is a "/"-separated chain of element names, read from a document's
// root and matched wherever that exact chain occurs, so a caller never has
// to spell out the wrapping <body> (or a channel container) a read happens
// to come back inside. "Osd/osdChannel/name" is the whole address; it does
// not change if the camera wraps its reply differently than expected.
//
// Kind is never a Go type. "warning" fields carry no XPath at all: they
// exist only to put text next to a control, which is how the
// UnsafeToRewrite label from Task 5 surfaces here without hiding the
// control it warns about.
type Field struct {
	Label string
	XPath string
	Kind  string
}

// Group is one curated section of the settings page, every field editing
// the same raw block.
//
// Blocks lists candidate blocks, by name exactly as baichuan.ConfigPair.Name
// gives it ("osd get", "get osd"), most preferred first. More than one
// candidate exists only for OSD: docs/control.md records "osd get" (44) and
// "get osd" (29) as two independently recovered pairs for the same
// settings, and normalising them onto one name is what pairs a write to the
// wrong message id (see ConfigPairs' own comment). Which one a given camera
// actually implements is not assumed; resolveGroupBlock discovers it by
// trying each candidate and taking the first that reads back 200.
//
// A defaults-only read such as "osd def get" (110) can never end up in
// Blocks by accident and do damage if it did: ConfigPairs only pairs a
// "X get" with a "X set" that shares its exact name, and no "osd def set"
// exists, so pairForName can never resolve a defaults read to a writable
// pair at all. That is what makes it safe for this list to be no more
// careful than "every writable candidate for this setting, most likely
// first": the factory-defaults trap is closed by construction, not by
// caution here.
type Group struct {
	Title  string
	Blocks []string
	Fields []Field
	// ConfirmReason, when non-empty, is shown next to the group's heading
	// and used as the confirmation prompt every one of its forms asks
	// through before it submits. Empty for a group like Picture and OSD,
	// where nothing switches anything a person or a camera can see happen;
	// set for Lights and IR, where every field is an emitter.
	ConfirmReason string
}

// unsafeToRewriteWarning is shown next to a control that edits a block
// UnsafeToRewrite flags, rather than hiding the control. It is worded for a
// page shown before any write happens, unlike write.go's own wording, which
// describes a write that already went out.
const unsafeToRewriteWarning = "unsafe to rewrite: writing this block, even with only one field changed, makes the camera reconfigure its pipeline and interrupts the stream."

// irLivesInImageWarning replaces an earlier, wrong claim that no writable
// infrared message exists at all. It does: InputAdvanceCfg/DayNight/IrcutMode
// is a real field in the same document isp get/isp set already carry (see
// isp.xml in testdata/livefixtures), alongside DayNight/mode and
// DayNight/Threshold. "fty ir_cut info" (371) is a different, read-only
// message; it was the one this codebase checked, which is how the wrong
// claim happened. The Infrared cut filter field below is that field,
// carrying the same unsafe-to-rewrite caveat as the rest of Image, because
// it shares Image's block.
const irLivesInImageWarning = "the infrared cut filter is changed from the Image group above: it shares that block and its stream-interruption caveat, not a separate one."

// groups declares the curated Picture and OSD surface plus Lights and IR:
// what a person actually changes, so they do not have to edit raw XML.
//
// Every XPath here comes from testdata/livefixtures, a real RLC-810A's own
// replies (osd2.xml, isp.xml, led.xml), not from guessing at Reolink's
// schema: an earlier version of this file invented paths such as
// "Osd/osdChannel/name" and "Isp/Isp/bright" that do not exist in any
// document this camera actually sends (the real names are
// "OsdChannelName/name" and "VideoInput/bright"), which is exactly the
// kind of field this codebase's own honesty rule (Task 8 and Finding 3)
// exists to catch rather than paper over with a blank input box.
//
// The status LED (message 209) is here because it is proven; the
// floodlight is not a Field at all, because its write lives over CGI, not
// a Baichuan block, and cameraPage carries it separately.
//
// Lights and IR sets ConfirmReason because everything in it is an emitter:
// nothing in Picture and OSD switches anything a person or a camera can
// see happen, and this group is the opposite of that.
// The ORDER of this list is load-bearing beyond the page's layout:
// camerapage_test.go's fake camera replies to config reads in exactly this
// order, and baichuan.Conn reads strictly forward with no rewind, so
// reordering these groups makes that test time out rather than say what
// changed.
func groups() []Group {
	return []Group{
		{
			Title: "Camera name and overlay",
			// "osd get" (44) is the live document on the RLC-810A this was
			// verified against; "get osd" (29) answered 405 there but is a
			// second, independently recovered pair for the same settings
			// on other firmware (see Group's own comment). Listed in this
			// order because "osd get" is the one seen live; a camera that
			// only implements "get osd" still gets a working group, just
			// discovered rather than assumed.
			Blocks: []string{"osd get", "get osd"},
			Fields: []Field{
				{Label: "Camera name", XPath: "OsdChannelName/name", Kind: "text"},
				{Label: "Show camera name", XPath: "OsdChannelName/enable", Kind: "toggle"},
				{Label: "Show timestamp", XPath: "OsdDatetime/enable", Kind: "toggle"},
			},
		},
		{
			Title:  "Image",
			Blocks: []string{"isp get"},
			Fields: []Field{
				{Label: "Brightness", XPath: "VideoInput/bright", Kind: "number"},
				{Label: "Contrast", XPath: "VideoInput/contrast", Kind: "number"},
				{Label: "Saturation", XPath: "VideoInput/saturation", Kind: "number"},
				{Label: "Day and night mode", XPath: "InputAdvanceCfg/DayNight/mode", Kind: "text"},
				// The infrared cut filter, corrected: see
				// irLivesInImageWarning's comment for why this exists at
				// all, despite an earlier version of this page claiming
				// it did not.
				{Label: "Infrared cut filter", XPath: "InputAdvanceCfg/DayNight/IrcutMode", Kind: "text"},
				{Label: unsafeToRewriteWarning, Kind: "warning"},
			},
		},
		{
			Title:         "Lights and IR",
			Blocks:        []string{"led get"},
			ConfirmReason: lightsConfirmReason,
			Fields: []Field{
				// LedState/state is a string enum ("auto" on the camera
				// this was verified against), not the 0/1 boolean this
				// field used to send: a numeric toggle would have written
				// "0" or "1" into a field the camera never uses those
				// values for. Free text, seeded from whatever string the
				// camera actually holds, so a person edits the camera's
				// own value rather than a guessed dropdown.
				{Label: "Status LED", XPath: "LedState/state", Kind: "text"},
				{Label: irLivesInImageWarning, Kind: "warning"},
			},
		},
	}
}

// pairForName finds the read/write pair whose firmware description is
// name, the same wording Group.Blocks and PairRow.Name already use. It is
// how one of a Group's Blocks candidates gets resolved back to the ids a
// handler needs to seed and write its fields.
func pairForName(name string) (baichuan.ConfigPair, bool) {
	for _, p := range baichuan.ConfigPairs() {
		if p.Name == name {
			return p, true
		}
	}
	return baichuan.ConfigPair{}, false
}

// setField locates the element xpath names and replaces only its text
// content with value.
//
// This edits bytes, not a decoded structure. Unmarshalling into a struct
// and re-marshalling is exactly what loses the XML declaration, reorders
// elements, changes whitespace, and drops every field this code does not
// model, and formatting is not cosmetic here: a compact document is
// accepted for TalkAbility and refused for TalkConfig. The camera supplies
// its own schema, including fields nobody here has ever modelled, and
// echoing its own document back byte for byte except for the one field
// being changed is what makes writes work on models nobody here has seen.
//
// An element that is not present is refused rather than invented: adding an
// element the camera never sent is composing a document, which is the
// thing this function exists to avoid.
func setField(doc []byte, xpath, value string) ([]byte, error) {
	if xpath == "" {
		return nil, fmt.Errorf("camctl: setField: empty xpath")
	}
	segs := strings.Split(xpath, "/")

	start, end, err := locateLeaf(doc, segs)
	if err != nil {
		return nil, fmt.Errorf("camctl: setField %q: %w", xpath, err)
	}

	var esc bytes.Buffer
	if err := xml.EscapeText(&esc, []byte(value)); err != nil {
		return nil, fmt.Errorf("camctl: setField %q: escaping value: %w", xpath, err)
	}

	out := make([]byte, 0, len(doc)+esc.Len())
	out = append(out, doc[:start]...)
	out = append(out, esc.Bytes()...)
	out = append(out, doc[end:]...)
	return out, nil
}

// span is one candidate match locateLeaf found: the byte range of an
// element's inner text.
type span struct{ start, end int }

// locateLeaf finds the byte span of the inner text of the element chain
// path names, matched as a trailing suffix of the document's current
// element stack, so a caller never has to spell out whatever the document
// wraps it in.
//
// It reads doc as a stream of tokens rather than building a tree, using
// only the decoder's own byte offsets to find where a match's content
// starts and ends. Nothing here reconstructs or reorders any byte setField
// hands back to its caller; the decoder is used purely to find offsets into
// the original, untouched bytes.
//
// Matching by suffix is what lets a caller's path skip an outer wrapper it
// was never told about, but the same rule means a short path such as
// "Isp/bright" matches every ancestor chain ending in those two names. A
// document with that pair once, say per channel, would make the first
// match a silent guess at which channel the caller meant: not a refusal,
// a wrong write, which is the one failure this codebase cares most about
// not producing. So this scans the whole document rather than stopping at
// the first hit, and refuses when more than one element matches, on the
// same principle that refuses an element that is not present at all: a
// path this code cannot resolve to exactly one element is not information
// this code has, and it must not guess.
func locateLeaf(doc []byte, path []string) (start, end int, err error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	var matches []span
	matchDepth := -1
	pendingStart := false
	var curStart int

	for {
		off := int(dec.InputOffset())
		tok, tokErr := dec.Token()
		if tokErr != nil {
			break
		}
		if pendingStart {
			curStart = off
			pendingStart = false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			// matchDepth == -1 also guards against a match already open:
			// once one is found, nothing inside its own subtree can start
			// a second, independent match, only a sibling or a later
			// element can.
			if matchDepth == -1 && suffixMatches(stack, path) {
				matchDepth = len(stack)
				pendingStart = true
			}
		case xml.EndElement:
			if matchDepth == len(stack) {
				// A self-closing element such as <enable/> gives the
				// decoder no separate open and close tags: the synthetic
				// EndElement it emits lands at the same offset as the end
				// of the StartElement token, which is indistinguishable by
				// offset alone from a genuinely empty "<a></a>". The bytes
				// tell them apart: only a self-closing tag ends in "/>".
				// Splicing at that offset would not insert text inside the
				// element at all, it would insert it after the element, as
				// a sibling, which is exactly the kind of invented content
				// this function exists to refuse rather than produce.
				if bytes.HasSuffix(doc[:curStart], []byte("/>")) {
					return 0, 0, fmt.Errorf("element is self-closing and has no text span to replace")
				}
				matches = append(matches, span{curStart, off})
				matchDepth = -1
			}
			stack = stack[:len(stack)-1]
		}
	}

	switch len(matches) {
	case 0:
		return 0, 0, fmt.Errorf("element not found in document")
	case 1:
		return matches[0].start, matches[0].end, nil
	default:
		return 0, 0, fmt.Errorf("%d elements match this path; refusing rather than guessing which one was meant", len(matches))
	}
}

// suffixMatches reports whether path is the trailing sequence of stack, so
// "Osd/osdChannel/name" matches whether the document wraps it in <body>,
// a channel container, or nothing at all.
func suffixMatches(stack, path []string) bool {
	if len(stack) < len(path) {
		return false
	}
	off := len(stack) - len(path)
	for i, name := range path {
		if stack[off+i] != name {
			return false
		}
	}
	return true
}

// fieldValue reads what setField would replace, without replacing it: the
// current text of the element xpath names, for seeding a form with what the
// camera actually holds right now. It shares locateLeaf with setField, so a
// value shown on the page and a value setField would refuse are found by
// exactly the same rule, including refusing an ambiguous match rather than
// guessing which one to display.
func fieldValue(doc []byte, xpath string) (string, bool) {
	if xpath == "" {
		return "", false
	}
	start, end, err := locateLeaf(doc, strings.Split(xpath, "/"))
	if err != nil {
		return "", false
	}
	return string(doc[start:end]), true
}

// resolveGroupBlock tries each of g.Blocks against conn, in order, and
// returns the first one that both names a real read/write pair and reads
// back 200: this is what discovers, rather than assumes, which of two
// independently recovered pairs for the same settings this camera actually
// implements (see Group's own comment on Blocks). failReason is empty
// exactly when pair and doc are usable; otherwise it names why every
// candidate was rejected, for a caller to show in place of a control.
func resolveGroupBlock(ctx context.Context, conn *baichuan.Conn, g Group) (pair baichuan.ConfigPair, doc []byte, failReason string) {
	var lastReason string
	for _, name := range g.Blocks {
		p, ok := pairForName(name)
		if !ok {
			lastReason = fmt.Sprintf("%q is not a known read/write pair", name)
			continue
		}
		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		d, status, err := baichuan.ReadConfig(readCtx, conn, p.Get)
		readCancel()
		if err != nil {
			lastReason = fmt.Sprintf("reading %s: %v", name, err)
			continue
		}
		if status != 200 {
			lastReason = fmt.Sprintf("%s: %s", name, baichuan.ExplainStatus(status))
			continue
		}
		return p, d, ""
	}
	if lastReason == "" {
		lastReason = "no candidate block is configured for this group"
	}
	return baichuan.ConfigPair{}, nil, fmt.Sprintf("this camera did not answer any of %s (%s)", strings.Join(g.Blocks, ", "), lastReason)
}

// curatedField reports whether block/xpath together name one of the fields
// groups() actually declares, and returns it.
//
// The settings form posts both as plain form values, and nothing about an
// HTTP request stops it from naming any block and any path. This is the
// check that a write from this form can only ever land on a field this
// page itself curated, never on whatever a request happens to claim.
func curatedField(block, xpath string) (Field, bool) {
	for _, g := range groups() {
		if !slices.Contains(g.Blocks, block) {
			continue
		}
		for _, f := range g.Fields {
			if f.XPath == xpath {
				return f, true
			}
		}
	}
	return Field{}, false
}

// serveApplySetting is the settings form's POST: read the field's block
// fresh, replace only the one field being changed with setField, and write
// the result through writeBlock, the same path serveWrite already uses for
// the raw view.
//
// This is not a second write path. Nothing here calls baichuan.WriteConfig
// itself; writeBlock does, which is what makes a curated edit inherit
// verification, the confirmed/accepted/refused vocabulary, the
// pre-write document capture a caller can restore from, and the
// unsafe-to-rewrite warning, all for free and all exactly as a raw edit
// already gets them.
//
// The field seeded on the page and the field a write targets are found by
// the identical rule on purpose: fieldValue and setField both resolve
// XPath through locateLeaf, so what an operator sees is what gets changed.
func (s *Server) serveApplySetting(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	block := r.FormValue("block")
	xpath := r.FormValue("xpath")
	value := r.FormValue("value")

	if _, ok := curatedField(block, xpath); !ok {
		http.Error(w, fmt.Sprintf("%s %s is not a curated field", block, xpath), http.StatusBadRequest)
		return
	}
	pair, ok := pairForName(block)
	if !ok {
		http.Error(w, fmt.Sprintf("%q is not a known block", block), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	// Read the block fresh, on its own connection, so setField starts from
	// what the camera holds right now rather than from whatever the page
	// happened to render it with a request or two ago. writeBlock reads
	// this same block again for its own Before capture; that second read
	// is not wasted work, it is what lets writeBlock report Before/After
	// without this handler having to hand it anything but a finished body.
	conn, err := s.dial(ctx, cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s before writing: %v", pair.Name, err), http.StatusBadGateway)
		return
	}
	readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(readCtx, conn, pair.Get)
	readCancel()
	conn.Close()
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s before writing: %v", pair.Name, err), http.StatusBadGateway)
		return
	}
	if status != 200 {
		http.Error(w, fmt.Sprintf("reading %s before writing: camera answered status %d", pair.Name, status), http.StatusBadGateway)
		return
	}

	body, err := setField(doc, xpath, value)
	if err != nil {
		// The inferred image XPaths in particular may not match this
		// model's actual schema. That must read as a refusal, the same
		// vocabulary a rejected write already uses, never as a silent
		// success: nothing was sent to the camera at all.
		s.render(w, "result.html", writeResultPage{
			Title:   fmt.Sprintf("%s: %s", cam.Name, block),
			Camera:  cam,
			Outcome: "refused",
			Detail:  fmt.Sprintf("could not apply %s to the current document: %v", xpath, err),
		})
		return
	}

	result, err := s.writeBlock(ctx, cam, pair.Set, body, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	page := writeResultPage{
		Title:   fmt.Sprintf("%s: %s", cam.Name, block),
		Camera:  cam,
		Outcome: result.Outcome,
		Detail:  result.Detail,
		Before:  string(result.Before),
		After:   string(result.After),
	}
	if len(result.Before) > 0 {
		page.RestoreAction = fmt.Sprintf("/cameras/%s/write/%d", cam.Name, pair.Set)
		page.RestoreParam = "body"
		page.RestoreValue = string(result.Before)
		page.RestoreHidden = map[string]string{"verify": "true"}
	}
	s.render(w, "result.html", page)
}
