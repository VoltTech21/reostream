// Package control serves the operator page: what every stream is doing,
// what the config says, what the log says, and a first run flow.
//
// It listens on its own socket, never the streaming one. The streaming
// listener is unauthenticated because a recorder points at it, and this
// page can read and write camera credentials, so the two must not share a
// port or a firewall rule.
package control

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"sync"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/server"
	"github.com/VoltTech21/reostream/internal/webui"
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

	// Probe asks a camera what it is, for the setup flow. Nil means
	// probeCamera; tests substitute their own so they never dial a real
	// camera.
	Probe func(ctx context.Context, addr, user, pass string) CameraReport
}

type Server struct {
	opts Options
	rend *webui.Renderer

	// tmpl is the same template set as rend, kept separately for
	// serveProbe, which renders a fragment for an HTMX-style swap and
	// deliberately bypasses the "layout" template rend.Render always
	// executes.
	tmpl *template.Template

	auth     webui.Auth
	sessions *webui.SessionStore

	// configMu serialises writeAndApply end to end: reading the previous
	// config, validating, writing the file, and reloading the fleet all
	// happen while it is held. The write and the apply must be atomic
	// together, not just each internally consistent, or two concurrent
	// POST /config requests can both pass validation, both write the file,
	// and then apply in the opposite order, leaving the file on disk and
	// the running fleet describing two different fleets with neither
	// operator told anything went wrong. Supervisor.Reload already
	// serialises itself with its own reloadMu, but that only protects the
	// apply step in isolation; it does nothing to stop the write from
	// happening out of order with it.
	//
	// Lock order: configMu is always acquired before any lock inside
	// Reloader.Reload (the supervisor's reloadMu, then its mu). Nothing
	// reachable from inside Reload ever tries to acquire configMu, so
	// there is no cycle.
	configMu sync.Mutex

	// done is closed by Close to release any handler blocked on a
	// long-lived connection, such as the log stream. http.Server.Shutdown
	// waits for active connections to finish and does not cancel their
	// request contexts, so without this signal a single open logs tab
	// would hold shutdown open for its full timeout.
	done     chan struct{}
	closeOne sync.Once

	// inFlightMu and inFlightProbes serialise setup probes against each
	// other, not just against the daemon's own config: probeGuarded's
	// config-membership check closes the window against an already
	// configured camera, but nothing else in-process stops two browser
	// tabs, a double click, or a browser retry from running two
	// probeCamera calls for the same unconfigured address at once, each
	// dialling up to four real connections to it. See beginProbe.
	inFlightMu     sync.Mutex
	inFlightProbes map[string]bool
}

func New(opts Options) *Server {
	rend, err := webui.NewRenderer(templateFS, "templates/*.html")
	if err != nil {
		// The template set is embedded at build time, so a parse failure
		// here is a bug in the binary itself, not something a caller can
		// recover from.
		panic(err)
	}
	sessions := webui.NewSessionStore()
	return &Server{
		opts: opts,
		rend: rend,
		tmpl: template.Must(template.ParseFS(templateFS, "templates/*.html")),
		auth: webui.Auth{
			Store:           sessions,
			Password:        opts.Password,
			AllowNoPassword: opts.AllowNoPassword,
			LoginPath:       "/login",
			// Distinct from camctl's own cookie name: see webui.Auth's own
			// comment for why the two surfaces cannot share one.
			CookieName: "reostream_control_session",
		},
		sessions:       sessions,
		done:           make(chan struct{}),
		inFlightProbes: make(map[string]bool),
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
	mux.Handle("GET /cameras", s.authed(http.HandlerFunc(s.serveCameras)))
	mux.Handle("POST /cameras", s.authed(http.HandlerFunc(s.saveCamera)))
	mux.Handle("GET /setup", s.authed(http.HandlerFunc(s.serveSetup)))
	mux.Handle("GET /setup/urls", s.authed(http.HandlerFunc(s.serveURLs)))
	mux.Handle("POST /setup/probe", s.authed(http.HandlerFunc(s.serveProbe)))
	mux.Handle("GET /assets/", s.authed(http.StripPrefix("/assets/",
		http.FileServer(http.FS(assetSub)))))
	// The live tiles need to fetch MPEG-TS from the same origin as this
	// page: mpegts.js pulls the stream over XHR, the streaming listener is
	// a different port and therefore a different origin, and that listener
	// must not gain CORS headers (it must not change at all -- a recorder
	// depends on it staying exactly as it is). Mounting the streaming
	// handler's own routes here, behind the same auth as everything else on
	// this page, gives tiles a same-origin URL without touching the
	// streaming listener or duplicating how it serves a hub. See
	// internal/control/tiles.go for the URLs this produces.
	if s.opts.Hubs != nil {
		streamSrv := server.New(s.opts.Hubs)
		mux.Handle("GET /stream/", s.authed(http.StripPrefix("/stream", streamSrv.StreamHandler())))
	}
	return mux
}

// render writes one page. data must carry a Title, which layout.html uses.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.rend.Render(w, "templates/"+name, data)
}

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	// A config that loaded fine but lists no cameras is a first run: send
	// the operator to setup rather than an empty table. A config that
	// failed to load is a different situation entirely and must not be
	// mistaken for "no cameras yet".
	if cfg, err := config.LoadRaw(s.opts.ConfigPath); err == nil && len(cfg.Cameras) == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, "dashboard.html", struct {
		Title  string
		Groups []cameraGroup
	}{Title: "Status", Groups: s.cameraGroups()})
}
