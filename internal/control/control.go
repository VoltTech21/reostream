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
	"io/fs"
	"log"
	"net/http"
	"sync"

	"github.com/VoltTech21/reostream/internal/server"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// assetSub drops the "assets" prefix so the URL and the file path match.
var assetSub, _ = fs.Sub(assetFS, "assets")

// Options is everything the control server needs from the rest of the
// daemon. Later tasks add fields; nothing here reaches back into streaming
// except through these.
type Options struct {
	Password        string
	AllowNoPassword bool
	Status          StatusSource
	Logs            *LogBuffer
	Hubs            server.HubSource
	StreamBase      string
	ConfigPath      string
	Supervisor      Reloader
}

type Server struct {
	opts Options
	tmpl *template.Template

	sessions *sessionStore

	// done is closed by Close to release any handler blocked on a
	// long-lived connection, such as the log stream. http.Server.Shutdown
	// waits for active connections to finish and does not cancel their
	// request contexts, so without this signal a single open logs tab
	// would hold shutdown open for its full timeout.
	done     chan struct{}
	closeOne sync.Once
}

func New(opts Options) *Server {
	return &Server{
		opts:     opts,
		tmpl:     template.Must(template.ParseFS(templateFS, "templates/*.html")),
		sessions: newSessionStore(),
		done:     make(chan struct{}),
	}
}

// Close releases any handler waiting on the server's done channel. Safe to
// call more than once.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.done)
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.authed(http.HandlerFunc(s.serveDashboard)))
	mux.Handle("GET /logs", s.authed(http.HandlerFunc(s.serveLogsPage)))
	mux.Handle("GET /logs/history", s.authed(http.HandlerFunc(s.serveLogHistory)))
	mux.Handle("GET /logs/stream", s.authed(http.HandlerFunc(s.serveLogStream)))
	mux.Handle("GET /config", s.authed(http.HandlerFunc(s.serveConfigPage)))
	mux.Handle("POST /config", s.authed(http.HandlerFunc(s.saveConfigPage)))
	mux.Handle("GET /assets/", s.authed(http.StripPrefix("/assets/",
		http.FileServer(http.FS(assetSub)))))
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

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard.html", struct {
		Title string
		Rows  []row
		Tiles []tile
	}{Title: "Status", Rows: s.rows(), Tiles: s.tiles()})
}
