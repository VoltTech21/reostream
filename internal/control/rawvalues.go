package control

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Raw values (Settings > Expert): every value in every settings document
// this camera hands over, each editable on its own. It is the same data
// Raw documents shows as XML, one field per row.

type rawValue struct {
	Path  string
	Value string
	// Repeated is a path that occurs more than once in its document (an
	// item in a list); setField refuses those, so the row is read only.
	Repeated bool
	// Curated names the curated setting this value is, when it is one.
	Curated string
}

type rawDoc struct {
	Title    string
	Get, Set uint32
	Unsafe   bool
	Values   []rawValue
}

type rawPage struct {
	Title string
	Head  camHead
	Cam   string
	Docs  []rawDoc
	Err   string
}

// leafValues lists a document's leaf elements by path, in document order.
func leafValues(doc []byte) []rawValue {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	var text strings.Builder
	hasChild := []bool{}
	var out []rawValue
	count := map[string]int{}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(hasChild) > 0 {
				hasChild[len(hasChild)-1] = true
			}
			stack = append(stack, t.Name.Local)
			hasChild = append(hasChild, false)
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if !hasChild[len(hasChild)-1] {
				path := stack
				if len(path) > 0 && path[0] == "body" {
					path = path[1:]
				}
				p := strings.Join(path, "/")
				if p != "" && !strings.HasSuffix(p, "channelId") {
					out = append(out, rawValue{Path: p, Value: strings.TrimSpace(text.String())})
					count[p]++
				}
			}
			stack = stack[:len(stack)-1]
			hasChild = hasChild[:len(hasChild)-1]
			text.Reset()
		}
	}
	for i := range out {
		out[i].Repeated = count[out[i].Path] > 1
	}
	return out
}

func (s *Server) serveRawValues(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	page := rawPage{Title: cam.Name + " raw values", Cam: cam.Name}
	page.Head = s.camHeadFor(r.Context(), cam, "settings", "raw")
	blocks, err := s.readBlocks(r.Context(), cam)
	if err != nil {
		page.Err = err.Error()
	}
	curated := map[string]string{}
	for _, g := range groups() {
		for _, f := range g.Fields {
			for _, p := range f.Paths() {
				curated[p] = f.Label
			}
		}
	}
	for _, row := range buildPairRows(blocks) {
		if !row.Editable {
			continue
		}
		d := rawDoc{Title: row.Title, Get: row.Get, Set: row.Set, Unsafe: row.Confidence == UnsafeToRewrite}
		for _, v := range leafValues([]byte(row.Seed)) {
			v.Curated = curated[v.Path]
			d.Values = append(d.Values, v)
		}
		if len(d.Values) > 0 {
			page.Docs = append(page.Docs, d)
		}
	}
	s.render(w, r, "raw.html", page)
}

// serveApplyRawValue writes one raw value: the document is read fresh,
// that one element changed, and the result written and verified the way
// every write here is.
func (s *Server) serveApplyRawValue(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	back := "/cameras/" + url.PathEscape(cam.Name) + "/raw"
	get, err := strconv.ParseUint(r.FormValue("get"), 10, 32)
	if err != nil {
		http.Error(w, "bad document id", http.StatusBadRequest)
		return
	}
	var pair baichuan.ConfigPair
	found := false
	for _, p := range baichuan.ConfigPairs() {
		if p.Get == uint32(get) {
			pair, found = p, true
		}
	}
	if !found {
		http.Error(w, "not a writable document", http.StatusBadRequest)
		return
	}
	xpath, value := r.FormValue("xpath"), r.FormValue("value")
	fail := func(msg string) {
		s.setFlash(w, r, Flash{Outcome: "refused", Message: xpath + ": " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, cam)
	if err != nil {
		fail("could not connect: " + err.Error())
		return
	}
	rctx, rcancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(rctx, conn, pair.Get)
	rcancel()
	conn.Close()
	if err != nil || status != 200 {
		fail(fmt.Sprintf("could not read the document (%v %d)", err, status))
		return
	}
	body, err := setField(doc, xpath, value)
	if err != nil {
		fail(err.Error())
		return
	}
	res, err := s.writeBlock(ctx, cam, pair.Set, body, true)
	if err != nil {
		fail(err.Error())
		return
	}
	s.setFlash(w, r, flashFor(cam.Name+": "+xpath, res))
	http.Redirect(w, r, back, http.StatusSeeOther)
}
