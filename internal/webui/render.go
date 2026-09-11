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
	t, err := r.base.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := t.ParseFS(r.fsys, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// The response is already partly written, so the log is the only
		// place this can go.
		log.Printf("webui: render %s: %v", page, err)
	}
}
