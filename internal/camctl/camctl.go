// Package camctl serves the camera control page: what a camera is, what it
// will tell you about itself, and what it will let you change.
//
// It runs as its own process on its own port, not as more routes on the
// streaming daemon's operator page. The split is by write target. The
// operator page edits reostream's own config and never writes to a camera.
// This writes to cameras, it is much larger, and a fault in it must never be
// able to take video down.
package camctl

import (
	"context"
	"embed"
	"net/http"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/webui"
)

//go:embed templates/*.html
var templateFS embed.FS

// Options is everything the camera control server needs.
type Options struct {
	Password        string
	AllowNoPassword bool
	ConfigPath      string
	Listen          string

	// Dial opens one connection to cam. Nil means baichuan.Dial against
	// cam's own address and credentials, which is what every real
	// deployment wants; a test supplies its own so it can probe a fake
	// camera instead of reaching for a real one.
	Dial func(ctx context.Context, cam Camera) (*baichuan.Conn, error)

	// CGIDial opens a CGI session to cam, for the handful of settings
	// reachable only over the camera's HTTP API: the floodlight, the
	// fisheye view modes, the dual lens stitch parameters. Nil means
	// cgi.Dial against cam's own address and credentials; a test supplies
	// its own so it can substitute a fake HTTP server instead of reaching
	// for a real camera.
	CGIDial func(cam Camera) (*cgi.Client, error)
}

// Server serves the camera control page.
type Server struct {
	opts     Options
	rend     *webui.Renderer
	auth     webui.Auth
	sessions *webui.SessionStore
}

// New builds a camera control server. It returns an error rather than
// panicking on a template parse failure so a caller can report it cleanly,
// rather than this package crashing a process that has not opened a
// listener yet.
func New(opts Options) (*Server, error) {
	rend, err := webui.NewRenderer(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	sessions := webui.NewSessionStore()
	return &Server{
		opts: opts,
		rend: rend,
		auth: webui.Auth{
			Store:           sessions,
			Password:        opts.Password,
			AllowNoPassword: opts.AllowNoPassword,
			LoginPath:       "/login",
		},
		sessions: sessions,
	}, nil
}

// Handler returns the routes for this page. Everything but the login routes
// runs behind auth.Wrap.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.auth.Wrap(http.HandlerFunc(s.serveFleet)))
	mux.Handle("GET /camera/{name}", s.auth.Wrap(http.HandlerFunc(s.serveCamera)))
	mux.Handle("GET /camera/{name}/blocks", s.auth.Wrap(http.HandlerFunc(s.serveBlocks)))
	mux.Handle("GET /camera/{name}/settings", s.auth.Wrap(http.HandlerFunc(s.serveSettings)))
	mux.Handle("POST /camera/{name}/settings", s.auth.Wrap(http.HandlerFunc(s.serveApplySetting)))
	mux.Handle("POST /camera/{name}/floodlight", s.auth.Wrap(http.HandlerFunc(s.serveApplyFloodlight)))
	mux.Handle("POST /camera/{name}/write/{id}", s.auth.Wrap(http.HandlerFunc(s.serveWrite)))
	return mux
}

// render writes one page. data must carry a Title, which layout.html uses.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.rend.Render(w, "templates/"+name, data)
}
