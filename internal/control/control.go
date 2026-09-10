// Package control serves the operator page: what every stream is doing,
// what the config says, what the log says, and a first run flow.
//
// It listens on its own socket, never the streaming one. The streaming
// listener is unauthenticated because a recorder points at it, and this
// page can read and write camera credentials, so the two must not share a
// port or a firewall rule.
package control

import (
	"embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed templates/*.html
var templateFS embed.FS

// Options is everything the control server needs from the rest of the
// daemon. Later tasks add fields; nothing here reaches back into streaming
// except through these.
type Options struct {
	Password        string
	AllowNoPassword bool
}

type Server struct {
	opts Options
	tmpl *template.Template

	sessions *sessionStore
}

func New(opts Options) *Server {
	return &Server{
		opts:     opts,
		tmpl:     template.Must(template.ParseFS(templateFS, "templates/*.html")),
		sessions: newSessionStore(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.authed(http.HandlerFunc(s.serveDashboard)))
	return mux
}

// render writes one page. data must carry a Title, which layout.html uses.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := t.ParseFS(templateFS, "templates/"+name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// The response is already partly written by here, so there is
		// nothing useful to send the client; the log is the only place this
		// can go.
		log.Printf("reostream: control: render %s: %v", name, err)
	}
}

// serveDashboard is replaced in Task 6. It exists now so auth has something
// to protect.
func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", struct {
		Title  string
		Failed bool
	}{Title: "Status"})
}
