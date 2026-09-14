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
	"errors"
	"fmt"
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
//go:embed templates/accounts.html templates/blocks.html templates/claim.html templates/camera.html templates/cameras.html templates/config.html templates/dashboard.html templates/fleetapply.html templates/layout.html templates/login.html templates/logs.html templates/probe.html templates/result.html templates/setup.html templates/time.html templates/urls.html
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

	// auth is the live auth state, not a value frozen at construction.
	// Claiming an install sets a password on a process that started with
	// none, and every route has to start requiring it immediately: a claim
	// that only took effect at the next restart would leave the "claimed"
	// install serving unauthenticated until somebody noticed, which is the
	// whole thing the claim exists to stop. So it is read per request
	// through authNow, under authMu, rather than captured once by
	// s.auth.Wrap at route registration time.
	//
	// authMu guards the two mutable fields, Password and AllowNoPassword.
	// Routes read them concurrently on every request; the claim handler
	// writes them once. Lock order: authMu is the innermost lock here --
	// the claim handler holds configMu while taking it, and nothing under
	// authMu ever reaches for configMu.
	//
	// claimSettled is the memo for claimed's config read: once the file
	// has said this install has an owner, that answer cannot change back,
	// so there is no reason to re-read it on every request afterwards.
	//
	// authLocked means the password in auth is a random one nobody can
	// present, set because the config names an environment variable that
	// is not set. It is tracked separately so that a lock does not read as
	// an owner: claimed keeps re-reading the file while it is set, which
	// is what lets a real password written afterwards take effect without
	// a restart.
	//
	// claimTok is the one-time token an unclaimed install will accept for
	// a claim. It is generated once in New, printed to the log by main,
	// and never written to disk -- a restart while the install is still
	// unclaimed makes a new one. A successful claim clears it: it
	// authorises exactly one claim. It lives under authMu with the rest of
	// the claim state because a claim writes it from one request while
	// other requests are reading it.
	authMu       sync.RWMutex
	auth         webui.Auth
	claimSettled bool
	authLocked   bool
	claimTok     string
	sessions     *webui.SessionStore

	// throttle is the shared failure lockout in front of the two routes
	// anyone who can reach this port may submit to without a session: the
	// claim token and the login password. Both are guessable one attempt
	// at a time and nothing else rate-limits them. See throttle.go.
	throttle *throttle

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
	// The token is generated for every process, not only for one that
	// turns out to be unclaimed: New cannot tell yet, claimed() answers
	// that from the config file at request time, and a token nobody needs
	// costs 16 bytes and is never printed. Generating it lazily would mean
	// deciding that question twice.
	tok, err := newClaimToken()
	if err != nil {
		return nil, fmt.Errorf("claim token: %w", err)
	}
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
		claimTok:       tok,
		sessions:       sessions,
		throttle:       newThrottle(),
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

// authNow returns a snapshot of the live auth state. A copy, so a caller
// can use Check, Wrap and CookieName without holding the lock while a
// handler runs.
func (s *Server) authNow() webui.Auth {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.auth
}

// wrap gates h on the auth state as it is at request time, which is what
// makes a claim take effect without a restart. webui.Auth.Wrap is still
// what decides; the only difference from calling it directly is that the
// Auth it decides with is read now rather than at registration.
func (s *Server) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.authNow().Wrap(h).ServeHTTP(w, r)
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
	// The claim routes sit outside the auth wrapper on purpose. While the
	// install is unclaimed there is no password for that wrapper to check,
	// and once it is claimed both routes answer 404 for everyone, which is
	// a better answer than a login redirect to a route that no longer
	// exists.
	mux.HandleFunc("GET /claim", s.serveClaimForm)
	mux.HandleFunc("POST /claim", s.serveClaim)
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.wrap(http.HandlerFunc(s.serveDashboard)))
	mux.Handle("GET /logs", s.wrap(http.HandlerFunc(s.serveLogsPage)))
	mux.Handle("GET /logs/history", s.wrap(http.HandlerFunc(s.serveLogHistory)))
	mux.Handle("GET /logs/stream", s.wrap(http.HandlerFunc(s.serveLogStream)))
	mux.Handle("GET /config", s.wrap(http.HandlerFunc(s.serveConfigPage)))
	mux.Handle("POST /config", s.wrap(http.HandlerFunc(s.saveConfigPage)))
	mux.Handle("GET /cameras", s.wrap(http.HandlerFunc(s.serveCameras)))
	mux.Handle("POST /cameras/add", s.wrap(http.HandlerFunc(s.saveCamera)))
	mux.Handle("GET /cameras/{name}", s.wrap(http.HandlerFunc(s.serveCamera)))
	mux.Handle("POST /cameras/{name}/settings", s.wrap(http.HandlerFunc(s.serveApplySetting)))
	mux.Handle("POST /cameras/{name}/floodlight", s.wrap(http.HandlerFunc(s.serveApplyFloodlight)))
	mux.Handle("GET /cameras/{name}/time", s.wrap(http.HandlerFunc(s.serveTime)))
	mux.Handle("POST /cameras/{name}/time", s.wrap(http.HandlerFunc(s.serveApplyTime)))
	// Accounts get a GET route only. No POST, PUT, PATCH or DELETE route
	// exists for /cameras/{name}/accounts anywhere in this package; see
	// accounts.go's top comment for why. Go's ServeMux answers 405 for a
	// method-specific pattern's path with no matching method, which is
	// what makes a request for any of those methods refuse itself without
	// this handler ever having to notice or check.
	mux.Handle("GET /cameras/{name}/accounts", s.wrap(http.HandlerFunc(s.serveAccounts)))
	mux.Handle("GET /cameras/{name}/advanced", s.wrap(http.HandlerFunc(s.serveBlocks)))
	mux.Handle("POST /cameras/{name}/write/{id}", s.wrap(http.HandlerFunc(s.serveWrite)))
	mux.Handle("GET /fleet/apply", s.wrap(http.HandlerFunc(s.serveFleetApplyForm)))
	mux.Handle("POST /fleet/apply/ntp", s.wrap(http.HandlerFunc(s.serveFleetApplyNTP)))
	mux.Handle("POST /fleet/apply/timezone", s.wrap(http.HandlerFunc(s.serveFleetApplyTimezone)))
	mux.Handle("GET /setup", s.wrap(http.HandlerFunc(s.serveSetup)))
	mux.Handle("GET /setup/urls", s.wrap(http.HandlerFunc(s.serveURLs)))
	mux.Handle("POST /setup/probe", s.wrap(http.HandlerFunc(s.serveProbe)))
	mux.Handle("GET /assets/", s.wrap(http.StripPrefix("/assets/",
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
		mux.Handle("GET /stream/", s.wrap(http.StripPrefix("/stream", streamSrv.StreamHandler())))
	}
	return s.claimGate(mux)
}

// render writes one page. data must carry a Title, which layout.html uses.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.rend.Render(w, "templates/"+name, data)
}

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	// Two ways to be a first run, and both belong at setup rather than an
	// empty status table: a config that loaded fine and lists no cameras,
	// and no config file at all. The second used to fall through to the
	// table, because an absent config was once fatal at startup and so
	// could not be seen here; it can now, since a fresh install starts
	// with no file and a claim is what creates one. Absent and unparseable
	// have split apart, and only unparseable keeps the old treatment: a
	// config somebody wrote that will not load is a different situation
	// entirely and must not be mistaken for "no cameras yet".
	//
	// Precedence against the claim screen is settled before this runs:
	// claimGate wraps the whole route table, so an unclaimed install never
	// reaches this handler at all. Anything arriving here is claimed, and
	// "no cameras yet" is the only first-run question left to ask.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if (err == nil && len(cfg.Cameras) == 0) || errors.Is(err, fs.ErrNotExist) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, "dashboard.html", struct {
		Title  string
		Groups []cameraGroup
	}{Title: "Status", Groups: s.cameraGroups()})
}
