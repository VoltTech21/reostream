// Curated settings: a small number of fields a person actually changes,
// picked out of the raw blocks blocks.html already exposes wholesale, so
// changing the camera's name or its timestamp overlay does not require
// editing an XML document by hand.
package camctl

import (
	"bytes"
	"encoding/xml"
	"fmt"
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
// Block is that block's name exactly as baichuan.ConfigPair.Name gives it
// ("osd get", "isp get"): the same wording blocks.html's PairRow.Name
// already renders, and the string pairForName resolves back to the Get and
// Set ids a handler needs to seed and write the group's fields.
type Group struct {
	Title  string
	Block  string
	Fields []Field
}

// unsafeToRewriteWarning is shown next to a control that edits a block
// UnsafeToRewrite flags, rather than hiding the control. It is worded for a
// page shown before any write happens, unlike write.go's own wording, which
// describes a write that already went out.
const unsafeToRewriteWarning = "unsafe to rewrite: writing this block, even with only one field changed, makes the camera reconfigure its pipeline and interrupts the stream."

// groups declares the curated Picture and OSD surface: what a person
// actually changes, so they do not have to edit raw XML.
//
// IR belongs to the Lights and IR group instead, not here, because it is an
// emitter and inherits that group's confirm-every-time rule; nothing in
// this group switches anything a person or a camera can see happen.
func groups() []Group {
	return []Group{
		{
			Title: "Camera name and overlay",
			Block: "osd get",
			Fields: []Field{
				{Label: "Camera name", XPath: "Osd/osdChannel/name", Kind: "text"},
				{Label: "Show camera name", XPath: "Osd/osdChannel/enable", Kind: "toggle"},
				{Label: "Show timestamp", XPath: "Osd/osdTime/enable", Kind: "toggle"},
			},
		},
		{
			Title: "Image",
			Block: "isp get",
			Fields: []Field{
				{Label: "Brightness", XPath: "Isp/Isp/bright", Kind: "number"},
				{Label: "Contrast", XPath: "Isp/Isp/contrast", Kind: "number"},
				{Label: "Saturation", XPath: "Isp/Isp/saturation", Kind: "number"},
				{Label: "Day and night switching", XPath: "Isp/Isp/dayNight", Kind: "text"},
				{Label: unsafeToRewriteWarning, Kind: "warning"},
			},
		},
	}
}

// pairForName finds the read/write pair whose firmware description is
// name, the same wording Group.Block and PairRow.Name already use. It is
// how a Group's Block gets resolved back to the ids a handler needs to seed
// and write its fields.
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

// locateLeaf finds the byte span of the inner text of the element chain
// path names, matched as a trailing suffix of the document's current
// element stack, so a caller never has to spell out whatever the document
// wraps it in.
//
// It reads doc as a stream of tokens rather than building a tree, using
// only the decoder's own byte offsets to find where the leaf's content
// starts and ends. Nothing here reconstructs or reorders any byte setField
// hands back to its caller; the decoder is used purely to find two offsets
// into the original, untouched bytes.
func locateLeaf(doc []byte, path []string) (start, end int, err error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	matchDepth := -1
	pendingStart := false

	for {
		off := int(dec.InputOffset())
		tok, tokErr := dec.Token()
		if tokErr != nil {
			break
		}
		if pendingStart {
			start = off
			pendingStart = false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if matchDepth == -1 && suffixMatches(stack, path) {
				matchDepth = len(stack)
				pendingStart = true
			}
		case xml.EndElement:
			if matchDepth == len(stack) {
				return start, off, nil
			}
			stack = stack[:len(stack)-1]
		}
	}
	return 0, 0, fmt.Errorf("element not found in document")
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
