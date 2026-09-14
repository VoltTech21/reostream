// Package control serves the daemon's one web page: what every stream is
// doing, what the config says, what the log says, a first run flow, and
// per camera control over Baichuan and CGI.
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
	"reflect"
	"sync"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/server"
	"github.com/VoltTech21/reostream/internal/webui"
)

// Listed explicitly, not templates/*.html: every page defines "body" by
// name, and each of the two aspects this server used to be (the operator
// page, the camera control page) defined its own "layout" too. Those two
// have since been unified into one layout.html; a wildcard glob is still
// refused here on purpose, because an explicit list is what makes an
// omitted template a build-time miss (it fails to compile, or the missing
// page 500s the first time it is hit) rather than a page that silently
// renders with the wrong chrome. Any template added under templates/ must
// be added to this list too.
//
//go:embed templates/accounts.html templates/blocks.html templates/camera.html templates/cameras.html templates/config.html templates/dashboard.html templates/fleetapply.html templates/layout.html templates/login.html templates/logs.html templates/probe.html templates/result.html templates/setup.html templates/time.html templates/urls.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// assetSub drops the "assets" prefix so the URL and the file path match.
var assetSub, _ = fs.Sub(assetFS, "assets")

// cameraOf finds a field named Camera on whatever page data v is, and
// returns it as a *Camera, or nil when the page has no such field. The
// sidebar template uses this to decide whether it is looking at a
// camera-specific page (the camera overview, its advanced blocks, time,
// accounts) or a fleet-wide one (the camera list, fleet apply, the
// dashboard, login): those pages carry no Camera at all, and a plain
// {{.Camera}} in the shared layout would fail to execute on them.
func cameraOf(v any) *Camera {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return nil
	}
	f := rv.FieldByName("Camera")
	if !f.IsValid() {
		return nil
	}
	cam, ok := f.Interface().(Camera)
	if !ok {
		return nil
	}
	return &cam
}

// Options is everything the control server needs from the rest of the
// daemon. Nothing here reaches back into streaming except through these.
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

	// Dial opens one connection to a camera for camera control (support,
	// abilities, the raw block probe, curated settings, writes). Nil means
	// baichuan.Dial against the camera's own address and credentials,
	// which is what every real deployment wants; a test supplies its own
	// so it can probe a fake camera instead of reaching for a real one.
	Dial func(ctx context.Context, cam Camera) (*baichuan.Conn, error)

	// CGIDial opens a CGI session to a camera, for the handful of settings
	// reachable only over the camera's HTTP API: the floodlight, the
	// clock. Nil means cgi.Dial against the camera's own address and
	// credentials; a test supplies its own so it can substitute a fake
	// HTTP server instead of reaching for a real camera.
	CGIDial func(cam Camera) (*cgi.Client, error)
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

// New builds the control server. It returns an error rather than panicking
// on a template parse failure so a caller can report it cleanly, rather
// than a process that has not opened a listener yet crashing outright.
func New(opts Options) (*Server, error) {
	// The sidebar needs the fleet on every page, not just the camera
	// list's own, so it is a template function bound to this daemon's
	// config path rather than a field every page struct would otherwise
	// have to carry. A page that cannot read the config renders its
	// sidebar without a fleet list rather than failing the whole render.
	funcs := template.FuncMap{
		"fleet": func() []Camera {
			cams, err := loadFleet(opts.ConfigPath)
			if err != nil {
				return nil
			}
			return cams
		},
		"cameraOf": cameraOf,
	}
	rend, err := webui.NewRenderer(templateFS, "templates/*.html", funcs)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	sessions := webui.NewSessionStore()
	return &Server{
		opts: opts,
		rend: rend,
		tmpl: tmpl,
		auth: webui.Auth{
			Store:           sessions,
			Password:        opts.Password,
			AllowNoPassword: opts.AllowNoPassword,
			LoginPath:       "/login",
			CookieName:      "reostream_control_session",
		},
		sessions:       sessions,
		done:           make(chan struct{}),
		inFlightProbes: make(map[string]bool),
	}, nil
}

// Close releases any handler waiting on the server's done channel. Safe to
// call more than once.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.done)
	})
}

// Handler returns the routes for this page. Everything but the login
// routes runs behind s.auth.Wrap: an unauthenticated route on this page
// reaches camera credentials or a camera itself.
//
// GET /cameras and GET /cameras/{name} are the heart of the merge this
// unified: the operator's config form and the camera control page's fleet
// list used to both answer "GET /cameras" with two different meanings (a
// config.toml entry, versus the physical device), and a person has one
// camera in their head, not two. /cameras is now the camera list, every
// configured camera with its stream state and a link into it; /cameras/{name}
// is that one camera, its stream state and video, then its curated
// settings.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.auth.Wrap(http.HandlerFunc(s.serveDashboard)))
	mux.Handle("GET /logs", s.auth.Wrap(http.HandlerFunc(s.serveLogsPage)))
	mux.Handle("GET /logs/history", s.auth.Wrap(http.HandlerFunc(s.serveLogHistory)))
	mux.Handle("GET /logs/stream", s.auth.Wrap(http.HandlerFunc(s.serveLogStream)))
	mux.Handle("GET /config", s.auth.Wrap(http.HandlerFunc(s.serveConfigPage)))
	mux.Handle("POST /config", s.auth.Wrap(http.HandlerFunc(s.saveConfigPage)))
	mux.Handle("GET /cameras", s.auth.Wrap(http.HandlerFunc(s.serveCameras)))
	mux.Handle("POST /cameras/add", s.auth.Wrap(http.HandlerFunc(s.saveCamera)))
	mux.Handle("GET /cameras/{name}", s.auth.Wrap(http.HandlerFunc(s.serveCamera)))
	mux.Handle("POST /cameras/{name}/settings", s.auth.Wrap(http.HandlerFunc(s.serveApplySetting)))
	mux.Handle("POST /cameras/{name}/floodlight", s.auth.Wrap(http.HandlerFunc(s.serveApplyFloodlight)))
	mux.Handle("GET /cameras/{name}/time", s.auth.Wrap(http.HandlerFunc(s.serveTime)))
	mux.Handle("POST /cameras/{name}/time", s.auth.Wrap(http.HandlerFunc(s.serveApplyTime)))
	// Accounts get a GET route only. No POST, PUT, PATCH or DELETE route
	// exists for /cameras/{name}/accounts anywhere in this package; see
	// accounts.go's top comment for why. Go's ServeMux answers 405 for a
	// method-specific pattern's path with no matching method, which is
	// what makes a request for any of those methods refuse itself without
	// this handler ever having to notice or check.
	mux.Handle("GET /cameras/{name}/accounts", s.auth.Wrap(http.HandlerFunc(s.serveAccounts)))
	mux.Handle("GET /cameras/{name}/advanced", s.auth.Wrap(http.HandlerFunc(s.serveBlocks)))
	mux.Handle("POST /cameras/{name}/write/{id}", s.auth.Wrap(http.HandlerFunc(s.serveWrite)))
	mux.Handle("GET /fleet/apply", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyForm)))
	mux.Handle("POST /fleet/apply/ntp", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyNTP)))
	mux.Handle("POST /fleet/apply/timezone", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyTimezone)))
	mux.Handle("GET /setup", s.auth.Wrap(http.HandlerFunc(s.serveSetup)))
	mux.Handle("GET /setup/urls", s.auth.Wrap(http.HandlerFunc(s.serveURLs)))
	mux.Handle("POST /setup/probe", s.auth.Wrap(http.HandlerFunc(s.serveProbe)))
	mux.Handle("GET /assets/", s.auth.Wrap(http.StripPrefix("/assets/",
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
		mux.Handle("GET /stream/", s.auth.Wrap(http.StripPrefix("/stream", streamSrv.StreamHandler())))
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
