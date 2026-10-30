package webui

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

// Renderer parses a set of page templates against a shared layout.
type Renderer struct {
	base *template.Template
	fsys fs.FS
}

// NewRenderer parses every template matching glob in fsys as the base set.
// funcs is registered before parsing, so a template body may call anything
// in it; nil is fine for a caller with no functions to add.
func NewRenderer(fsys fs.FS, glob string, funcs template.FuncMap) (*Renderer, error) {
	t, err := template.New("").Funcs(funcs).ParseFS(fsys, glob)
	if err != nil {
		return nil, err
	}
	return &Renderer{base: t, fsys: fsys}, nil
}

// Render writes one page. The base set is cloned and the page parsed into
// the clone on every call, because each page defines its own "body" block
// and a single shared set would have them collide.
func (r *Renderer) Render(w http.ResponseWriter, page string, data any) {
	r.RenderWith(w, page, data, nil)
}

// RenderWith is Render with funcs replacing, for this one page, functions
// of the same names registered at NewRenderer. It is how a value that
// belongs to one request, such as the path prefix the page is served
// under, reaches every template without every page's data carrying it.
// The clone is this call's own, so this is safe while other requests
// render with other values.
func (r *Renderer) RenderWith(w http.ResponseWriter, page string, data any, funcs template.FuncMap) {
	t, err := r.base.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if funcs != nil {
		t.Funcs(funcs)
	}
	if _, err := t.ParseFS(r.fsys, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page here is a reading of a camera's current settings, so a
	// stored copy is a wrong one the moment anything writes. Without this
	// the browser is free to answer a Back navigation from its own cache,
	// and the page that comes back shows the values from before the write
	// -- which reads as "my change did not save" when it did. no-store
	// rather than no-cache: there is nothing to revalidate, and these
	// pages are cheap to rebuild.
	w.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// The response is already partly written, so the log is the only
		// place this can go.
		log.Printf("webui: render %s: %v", page, err)
	}
}
