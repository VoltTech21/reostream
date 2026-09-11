package camctl

import (
	"bytes"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

func TestSetFieldReplacesOneElementAndLeavesTheRestByteIdentical(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<body><Osd><channelId>0</channelId><osdChannel><enable>1</enable><name>front</name></osdChannel></Osd></body>`)
	got, err := setField(doc, "Osd/osdChannel/name", "lounge")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("<name>lounge</name>")) {
		t.Fatalf("field not replaced:\n%s", got)
	}
	// Everything else must survive untouched, including the declaration and
	// fields this code has never heard of. The camera supplies its own
	// schema and echoing it back is what makes writes work on models nobody
	// here has seen.
	if !bytes.Contains(got, []byte(`<?xml version="1.0" encoding="UTF-8" ?>`)) {
		t.Fatal("the XML declaration was lost")
	}
	if !bytes.Contains(got, []byte("<channelId>0</channelId>")) {
		t.Fatal("an unrelated field was lost")
	}
	if !bytes.Contains(got, []byte("<enable>1</enable>")) {
		t.Fatal("a sibling field was lost")
	}
}

func TestSetFieldRefusesAnAbsentElementRatherThanInventingIt(t *testing.T) {
	doc := []byte("<body><Osd></Osd></body>")
	if _, err := setField(doc, "Osd/osdChannel/name", "x"); err == nil {
		t.Fatal("setField invented an element the camera never sent")
	}
}

func TestSetFieldMatchesRegardlessOfWrappingDepth(t *testing.T) {
	// The same path must find the leaf whether or not the document wraps it
	// in an outer element setField was never told about, since a handler
	// only ever knows the path relative to the field it cares about, not
	// every possible wrapper a camera might choose to send it inside.
	bare := []byte("<Osd><osdChannel><name>front</name></osdChannel></Osd>")
	got, err := setField(bare, "Osd/osdChannel/name", "lounge")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("<name>lounge</name>")) {
		t.Fatalf("field not replaced in unwrapped document:\n%s", got)
	}
}

func TestSetFieldEscapesTheValue(t *testing.T) {
	doc := []byte("<Osd><osdChannel><name>front</name></osdChannel></Osd>")
	got, err := setField(doc, "Osd/osdChannel/name", "A & B < C")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("<name>A & B < C</name>")) {
		t.Fatal("an unescaped value was written into the document, producing invalid XML")
	}
	if !bytes.Contains(got, []byte("&amp;")) || !bytes.Contains(got, []byte("&lt;")) {
		t.Fatalf("value was not escaped:\n%s", got)
	}
}

// TestImageGroupIsMarkedUnsafeToRewrite pins Group.Block to an id
// confidenceOf and baichuan.UnsafeToRewrite actually agree is unsafe, and
// pins that a warning field rides along with it, so a future edit that
// drops either the block or the warning fails here rather than only
// showing up as a missing warning on a page nobody re-reads by eye.
func TestImageGroupIsMarkedUnsafeToRewrite(t *testing.T) {
	found := false
	for _, g := range groups() {
		if g.Block != "isp get" {
			continue
		}
		found = true
		pair, ok := pairForName(g.Block)
		if !ok {
			t.Fatalf("group %q names block %q, which is not a known read/write pair", g.Title, g.Block)
		}
		if !baichuan.UnsafeToRewrite(pair.Set) {
			t.Fatalf("group %q's block %q is not unsafe to rewrite, but this test assumed it was", g.Title, g.Block)
		}
		hasWarning := false
		for _, f := range g.Fields {
			if f.Kind == "warning" {
				hasWarning = true
			}
		}
		if !hasWarning {
			t.Fatalf("group %q edits an unsafe-to-rewrite block with no warning field", g.Title)
		}
	}
	if !found {
		t.Fatal("no group names the isp get block, so this test asserted nothing")
	}
}

func TestPairForNameFindsTheOsdPair(t *testing.T) {
	pair, ok := pairForName("osd get")
	if !ok {
		t.Fatal("pairForName did not find the osd get pair")
	}
	if pair.Get != 44 || pair.Set != 45 {
		t.Fatalf("osd get pair is {Get:%d Set:%d}, want {Get:44 Set:45}", pair.Get, pair.Set)
	}
}
