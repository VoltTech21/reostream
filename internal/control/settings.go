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
	"net/url"
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

	// YPath is the second element a control that edits a PAIR of elements
	// changes alongside XPath. It is empty for every ordinary field, which
	// edits exactly one element.
	//
	// It exists for Kind "position": an overlay's corner is not one value
	// on these cameras, it is topLeftX and topLeftY, and either one on its
	// own is a corner nobody chose. XPath addresses the X, YPath the Y,
	// and serveApplySetting applies both inside a single read-modify-write
	// -- see parseEdits.
	YPath string

	// Slug is a stable id for the field's row. Governs, on a toggle, is
	// the Slug of the row that toggle switches on and off.
	Slug    string
	Governs string

	// Choices, on a Kind "choice" field, are the values offered as stops.
	// Only the value the camera was seen holding is observed; the rest are
	// the firmware's documented spellings. The camera's own value is always
	// offered too, so a camera holding something unlisted keeps it.
	Choices []Choice
}

// Choice is one stop on a Kind "choice" field.
type Choice struct {
	Value string
	Label string
}

// Paths lists every element this field edits, primary first: one path for
// an ordinary field, two for a position. A "warning" field, which carries
// no XPath at all, edits nothing and lists nothing.
func (f Field) Paths() []string {
	if f.XPath == "" {
		return nil
	}
	if f.YPath == "" {
		return []string{f.XPath}
	}
	return []string{f.XPath, f.YPath}
}

// FormXPath is what the settings form posts as this field's xpath: the one
// element it edits, or the comma-separated list of elements it changes
// together. A single-element field posts exactly the string it always did,
// so nothing about an existing form's wire format changes.
func (f Field) FormXPath() string {
	return strings.Join(f.Paths(), ",")
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
	// InterruptsStream tags the heading of a group whose write makes the
	// camera rebuild its pipeline. Blurb is the one plain line under it.
	InterruptsStream bool
	Blurb            string
}

// The overlay corner table, and how it was established.
//
// Each overlay in the "osd get" block (message 44) carries topLeftX and
// topLeftY next to its enable flag:
//
//	<OsdDatetime>    <enable>1</enable> <topLeftX>1</topLeftX>     <topLeftY>1</topLeftY>     ...
//	<OsdChannelName> <enable>0</enable> <topLeftX>65536</topLeftX> <topLeftY>65536</topLeftY> ...
//
// These are NOT pixel coordinates, despite the names. Read across four
// live cameras on one fleet, the two fields only ever held 1 or 65536. One
// camera had its timestamp at topLeftX=65536, topLeftY=1 while the other
// three had 1,1; pulling a frame off each stream and looking at it settled
// what that meant: that camera's clock is drawn in the TOP RIGHT and the
// others' in the top left. Every camera name on that fleet sat at
// 65536,65536, and every camera name was drawn bottom right. So the pair
// is a corner encoding, 1 meaning "against this edge" and 65536 "against
// the far edge":
//
//	corner        topLeftX  topLeftY
//	top left      1         1
//	top right     65536     1
//	bottom left   1         65536
//	bottom right  65536     65536
//
// This came from hardware, not from documentation: no Reolink document
// this project has seen says any of it, and it is written down here so
// nobody has to pull frames off four cameras a second time to recover it.
//
// The honest limit: only those two values were ever OBSERVED. Whether a
// camera would accept, say, 32768 and centre an overlay is simply not
// known, and this control does not pretend to know by offering it. An
// operator who wants to find out can still write topLeftX and topLeftY to
// anything from the advanced page, where every field of the raw block
// stays editable; and a camera already reporting a pair this table has no
// name for keeps it, shown as itself, rather than being snapped to the
// nearest corner (see cameraPage.Position).
const (
	osdNearEdge = "1"
	osdFarEdge  = "65536"
)

// osdCorner is one row of the table above: the wording a person picks and
// the pair of values picking it writes.
type osdCorner struct {
	Label string
	X, Y  string
}

// osdCorners is that table, in reading order. A function rather than a
// package variable for the same reason groups() is one: nothing can mutate
// a caller's copy.
func osdCorners() []osdCorner {
	return []osdCorner{
		{"top left", osdNearEdge, osdNearEdge},
		{"top right", osdFarEdge, osdNearEdge},
		{"bottom left", osdNearEdge, osdFarEdge},
		{"bottom right", osdFarEdge, osdFarEdge},
	}
}

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
			Blurb: "What the camera burns into the picture. Safe to change while streaming.",
			// "osd get" (44) is the live document on the RLC-810A this was
			// verified against; "get osd" (29) answered 405 there but is a
			// second, independently recovered pair for the same settings
			// on other firmware (see Group's own comment). Listed in this
			// order because "osd get" is the one seen live; a camera that
			// only implements "get osd" still gets a working group, just
			// discovered rather than assumed.
			Blocks: []string{"osd get", "get osd"},
			Fields: []Field{
				{Label: "Camera name", XPath: "OsdChannelName/name", Kind: "text", Slug: "osd-name"},
				{Label: "Show camera name", XPath: "OsdChannelName/enable", Kind: "toggle", Slug: "osd-name-show", Governs: "osd-name-position"},
				// Two elements, one control. See the corner table above for
				// what the values mean and how that was established.
				{Label: "Camera name position", XPath: "OsdChannelName/topLeftX", YPath: "OsdChannelName/topLeftY", Kind: "position", Slug: "osd-name-position"},
				{Label: "Show timestamp", XPath: "OsdDatetime/enable", Kind: "toggle", Slug: "osd-time-show", Governs: "osd-time-position"},
				{Label: "Timestamp position", XPath: "OsdDatetime/topLeftX", YPath: "OsdDatetime/topLeftY", Kind: "position", Slug: "osd-time-position"},
			},
		},
		{
			Title:            "Image",
			Blocks:           []string{"isp get"},
			InterruptsStream: true,
			Blurb:            "How the sensor exposes and colors the picture. Every save briefly interrupts the stream.",
			Fields: []Field{
				{Label: "Brightness", XPath: "VideoInput/bright", Kind: "number", Slug: "image-brightness"},
				{Label: "Contrast", XPath: "VideoInput/contrast", Kind: "number", Slug: "image-contrast"},
				{Label: "Saturation", XPath: "VideoInput/saturation", Kind: "number", Slug: "image-saturation"},
				{Label: "Day and night mode", XPath: "InputAdvanceCfg/DayNight/mode", Kind: "choice", Slug: "image-daynight",
					Choices: []Choice{{"auto", "Auto"}, {"color", "Color"}, {"blackAndWhite", "Black & white"}}},
				// The infrared cut filter, corrected: see
				// irLivesInImageWarning's comment for why this exists at
				// all, despite an earlier version of this page claiming
				// it did not.
				// A reading, not a control. Every camera on the fleet this
				// was built against reports "ir" and nothing has established
				// what else the firmware accepts, so a dropdown would be a
				// guess presented as a fact and a text box asks the operator
				// to guess instead. This field moves a physical part, so
				// neither is good enough. Advanced edits it by xpath for
				// anyone who does know a value to try.
				{Label: "Infrared cut filter", XPath: "InputAdvanceCfg/DayNight/IrcutMode", Kind: "reading", Slug: "image-ircut"},
			},
		},
		{
			Title:         "Lights and IR",
			Blocks:        []string{"led get"},
			ConfirmReason: lightsConfirmReason,
			Blurb:         "Lights on the camera itself. Each change asks again first.",
			Fields: []Field{
				// LedState/state is a string enum ("auto" on the camera
				// this was verified against), not the 0/1 boolean this
				// field used to send: a numeric toggle would have written
				// "0" or "1" into a field the camera never uses those
				// values for. Stops for the firmware's spellings, and the
				// camera's own value is always offered as well, so a camera
				// holding something else is shown it rather than snapped.
				{Label: "Status LED", XPath: "LedState/state", Kind: "choice", Slug: "led-state",
					Choices: []Choice{{"auto", "Auto"}, {"open", "On"}, {"close", "Off"}}},
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
		return nil, fmt.Errorf("control: setField: empty xpath")
	}
	segs := strings.Split(xpath, "/")

	start, end, err := locateLeaf(doc, segs)
	if err != nil {
		return nil, fmt.Errorf("control: setField %q: %w", xpath, err)
	}

	var esc bytes.Buffer
	if err := xml.EscapeText(&esc, []byte(value)); err != nil {
		return nil, fmt.Errorf("control: setField %q: escaping value: %w", xpath, err)
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
			// Every path the field edits, not only its primary one: a
			// position field's topLeftY is exactly as curated as its
			// topLeftX, and a write naming it must pass this check rather
			// than be rejected as something the page never declared.
			for _, p := range f.Paths() {
				if p == xpath {
					return f, true
				}
			}
		}
	}
	return Field{}, false
}

// edit is one element a write changes: where it is and what it becomes.
type edit struct {
	XPath string
	Value string
}

// parseEdits turns the settings form's xpath and value into the list of
// element changes one write must carry.
//
// One control, one form, one write -- but some controls are more than one
// element. An overlay's position is topLeftX AND topLeftY, and a browser
// select can only post a single value, so the form names its elements in
// xpath as a comma-separated list (Field.FormXPath) and posts the values in
// the same order. A form naming one element posts exactly the two plain
// strings it always did and comes back out of here as exactly one edit.
//
// The xpath list governs the split, never the value. A single-element write
// is not split at all, so a camera name containing a comma still arrives
// whole; a two-element write splits the value into exactly two parts. A
// value list that does not have one part per path is refused rather than
// padded: half a position is a corner nobody chose.
func parseEdits(xpathList, valueList string) ([]edit, error) {
	paths := strings.Split(xpathList, ",")
	if len(paths) == 1 {
		return []edit{{XPath: paths[0], Value: valueList}}, nil
	}
	values := strings.SplitN(valueList, ",", len(paths))
	if len(values) != len(paths) {
		return nil, fmt.Errorf("this control changes %d fields together but %d values were posted", len(paths), len(values))
	}
	edits := make([]edit, len(paths))
	for i := range paths {
		edits[i] = edit{XPath: paths[i], Value: values[i]}
	}
	return edits, nil
}

// serveApplySetting is the settings form's POST: read the field's block
// fresh, replace the field or fields being changed with setField, and write
// the result through writeBlock, the same path serveWrite already uses for
// the raw view.
//
// One read, every change, one write. A control that edits two elements at
// once (an overlay's corner) must not become two read-modify-write round
// trips: a camera that accepted the first and refused the second would
// leave the overlay in a corner nobody picked. So every edit is applied to
// the one document this handler read, and if any of them cannot be applied
// nothing is sent at all -- setField is pure, so the whole document is
// either finished or abandoned before a single byte goes to the camera.
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

	edits, err := parseEdits(r.FormValue("xpath"), r.FormValue("value"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Every posted path is checked against groups() before anything is
	// read, for the same reason one always was: nothing about an HTTP
	// request stops it naming any path at all, and a write from this form
	// may only ever land on a field this page itself curated. A list makes
	// that check a loop, not a weaker rule.
	var field Field
	for i, e := range edits {
		f, ok := curatedField(block, e.XPath)
		if !ok {
			http.Error(w, fmt.Sprintf("%s %s is not a curated field", block, e.XPath), http.StatusBadRequest)
			return
		}
		if i == 0 {
			field = f
		}
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

	// Where this write reports back to. The camera page is where a curated
	// setting lives, but the status page's quick overlay switches post
	// through this same handler, and an operator toggling one on the fleet
	// view expects to still be on the fleet view afterwards; returnTo lets
	// that form say where it came from, and ignores anything that is not a
	// path on this page.
	back := returnTo(r, "/cameras/"+url.PathEscape(cam.Name))
	subject := fmt.Sprintf("%s: %s", cam.Name, lowerFirst(field.Label))

	// Every edit onto the one document just read, in order. Each setField
	// returns a fresh slice, so a later one that fails leaves the earlier
	// ones nowhere but in a local nobody sends.
	body := doc
	for _, e := range edits {
		next, setErr := setField(body, e.XPath, e.Value)
		if setErr != nil {
			// The inferred image XPaths in particular may not match this
			// model's actual schema. That must read as a refusal, the same
			// vocabulary a rejected write already uses, never as a silent
			// success: nothing was sent to the camera at all. For a
			// multi-element control this is also what keeps it whole --
			// one unappliable half refuses the entire change rather than
			// writing the other half on its own.
			s.setFlash(w, r, flashFor(subject, WriteResult{
				Outcome: "refused",
				Detail:  fmt.Sprintf("could not apply %s to the current document: %v", e.XPath, setErr),
			}))
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		body = next
	}

	result, err := s.writeBlock(ctx, cam, pair.Set, body, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Post/redirect/get rather than rendering the write's own result page.
	// A refresh after this is a plain GET of the page the setting lives
	// on, not a second write to the camera, and the operator is left
	// looking at the control they just changed rather than at a wall of
	// before/after XML whose only way onward was "restore previous".
	flash := flashFor(subject, result)
	if len(result.Before) > 0 {
		// The same verified restore result.html offers, built the same
		// way: the document this block held before the write, sent back
		// through /write/{id} with verify on. Only the presentation
		// changes.
		flash.UndoAction = fmt.Sprintf("/cameras/%s/write/%d", cam.Name, pair.Set)
		flash.UndoParam = "body"
		flash.UndoValue = string(result.Before)
		// Carry the page this write came from into the undo itself, so
		// pressing undo lands back here with a banner rather than on the
		// block editor's before/after page.
		flash.UndoHidden = map[string]string{"verify": "true", "return": back}
	}
	s.setFlash(w, r, flash)
	http.Redirect(w, r, back, http.StatusSeeOther)
}
