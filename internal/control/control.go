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
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

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
//go:embed templates/password.html templates/accounts.html templates/blocks.html templates/claim.html templates/discover.html templates/addfound.html templates/camera.html templates/camshell.html templates/appsettings.html templates/about.html templates/nvr.html templates/events.html templates/raw.html templates/recordings.html templates/cameras.html templates/config.html templates/dashboard.html templates/wall.html templates/flash.html templates/layout.html templates/login.html templates/logs.html templates/probe.html templates/result.html templates/setup.html templates/time.html templates/urls.html templates/view.html templates/align.html
var templateFS embed.FS

// Explicit, for the same reason the template list above is, plus one of
// its own: everything under assets/ is served WITHOUT authentication, so a
// file that lands in this directory is world readable the moment it exists.
// A wildcard makes that the default for anything anyone ever drops here; a
// list makes publishing a file a decision somebody typed. Demonstrated
// before this was changed: a creds.toml placed in this directory was served
// to an unauthenticated request with every test still green.
//
//go:embed assets/style.css assets/dashboard.js assets/settings.js assets/snapshot.js assets/fisheye.js assets/fisheyeview.js assets/align.js assets/encoding.js assets/talk.js assets/live.js assets/rec.js assets/schedule.js assets/zone.js assets/mask.js assets/rules.js assets/sound.js assets/mpegts.js assets/mpegts.js.LICENSE.txt assets/LICENSE-mpegts.txt assets/README.md
var assetFS embed.FS

// assetSub drops the "assets" prefix so the URL and the file path match.
var assetSub, _ = fs.Sub(assetFS, "assets")

// assetETags maps each embedded asset to a hash of its bytes. The browser
// may keep an asset in cache, but "no-cache" makes it revalidate the ETag
// every time, so a new build -- with new CSS or JS -- is picked up at once
// rather than hours later, while an unchanged file still answers 304 and is
// not re-sent. Without this the embed FS has no modtime, so the browser
// caches heuristically and shows a stale page after an update.
var assetETags = func() map[string]string {
	m := map[string]string{}
	fs.WalkDir(assetSub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, e := fs.ReadFile(assetSub, p); e == nil {
			sum := sha256.Sum256(b)
			m[p] = `"` + hex.EncodeToString(sum[:10]) + `"`
		}
		return nil
	})
	return m
}()

// serveAsset serves one embedded asset with a content ETag and revalidation,
// so an updated build is never hidden behind a cached stylesheet.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path // already stripped of the /assets/ prefix
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(assetSub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if et := assetETags[name]; et != "" {
		w.Header().Set("ETag", et)
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

// cameraOf finds a field named Camera on whatever page data v is, and
// returns it as a *Camera, or nil when the page has no such field. The
// sidebar template uses this to decide whether it is looking at a
// camera-specific page (the camera overview, its advanced blocks, time,
// accounts) or a fleet-wide one (the camera list, the dashboard,
// login): those pages carry no Camera at all, and a plain
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

	// The addresses the daemon is actually running on, after -listen,
	// -rtsp-listen and -control-listen. A claim writes these into the
	// config it creates, so the file agrees with how reostream was
	// started. Empty means the default for that listener.
	Listen        string
	RTSPListen    string
	ControlListen string

	// BasePath serves the page under a path prefix, for a reverse proxy
	// that puts it somewhere other than "/". HAIngress trusts Home
	// Assistant's ingress proxy to say what the prefix is and to have
	// signed the user in already. See basepath.go for both.
	BasePath  string
	HAIngress bool

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

	// batteries is the last battery level each battery camera reported.
	batteries batteryCache

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
	// authorizes exactly one claim. It lives under authMu with the rest of
	// the claim state because a claim writes it from one request while
	// other requests are reading it.
	authMu       sync.RWMutex
	auth         webui.Auth
	claimSettled bool
	authLocked   bool
	claimTok     string
	sessions     *webui.SessionStore

	// There is no rate limit on POST /login, and none on POST /claim
	// either. Behind docker-proxy every request arrives from one address,
	// so a source-keyed limit cannot tell the attacker from the operator
	// and can only refuse the operator, delay the operator, or bound
	// nothing. Four of them were built and measured before that was
	// accepted; serveLogin records what each one did. The password carries
	// the security instead: see claimPasswordMinLength.

	// configMu serializes writeAndApply end to end: reading the previous
	// config, validating, writing the file, and reloading the fleet all
	// happen while it is held. The write and the apply must be atomic
	// together, not just each internally consistent, or two concurrent
	// POST /config requests can both pass validation, both write the file,
	// and then apply in the opposite order, leaving the file on disk and
	// the running fleet describing two different fleets with neither
	// operator told anything went wrong. Supervisor.Reload already
	// serializes itself with its own reloadMu, but that only protects the
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

	// flashMu, flashes and flashNow are the one-line banner a write leaves
	// behind for the page it redirects to; see flash.go for why it is held
	// here rather than passed through the redirect URL. Every page handler
	// and every write handler touches this map concurrently, so it has its
	// own lock. flashNow is nil in a real server and set only by a test
	// that needs to move time rather than sleep out flashTTL.
	//
	// Lock order: flashMu is innermost. Nothing reachable while it is held
	// takes any other lock, and setFlash reads the cookie name through
	// authNow (which takes authMu) BEFORE acquiring it, not while holding
	// it.
	flashMu  sync.Mutex
	flashes  map[string]flashEntry
	flashNow func() time.Time

	// inFlightMu and inFlightProbes serialize setup probes against each
	// other, not just against the daemon's own config: probeGuarded's
	// config-membership check closes the window against an already
	// configured camera, but nothing else in-process stops two browser
	// tabs, a double click, or a browser retry from running two
	// probeCamera calls for the same unconfigured address at once, each
	// dialling up to four real connections to it. See beginProbe.
	//
	// sweeping is the same idea for the network sweep on the setup page,
	// except there is one slot for the whole daemon rather than one per
	// address: a probe's hazard is scoped to the camera it dials, but a
	// sweep's is the load it puts on the wire, and two sweeps of two
	// different ranges put twice as much of it on the same wire. It lives
	// under inFlightMu with the probe set because it is the same claim
	// discipline; see beginSweep in discover.go.
	inFlightMu     sync.Mutex
	inFlightProbes map[string]bool
	sweeping       bool

	// sweepPort is the port the network sweep dials, 0 meaning 9000.
	// Tests set it so they can sweep loopback against a listener they
	// bound themselves; a real install never sets it, because a Reolink
	// camera never serves Baichuan anywhere else.
	sweepPort int

	// silentMu guards silentUntil: when a camera last timed out on a dial
	// from this page, keyed by address and uid. See dial.
	silentMu sync.Mutex

	// recorders remembers which addresses answered as an NVR or Home Hub
	// rather than a camera (DeviceInfo type), so channel 0 behind one is
	// still told apart from a camera on its own. See behindRecorder.
	// ptzHeld is each camera's kept-open pan/tilt connection; see
	// ptzhold.go.
	ptzMu   sync.Mutex
	ptzHeld map[string]*ptzHold

	recorderMu  sync.Mutex
	recorders   map[string]bool
	silentUntil map[string]time.Time
}

// templateFuncs is the function set every page is rendered with.
//
// The sidebar needs the fleet on every page, not just the camera list's
// own, so it is a template function bound to this daemon's config path
// rather than a field every page struct would otherwise have to carry. A
// page that cannot read the config renders its sidebar without a fleet
// list rather than failing the whole render.
func templateFuncs(opts Options) template.FuncMap {
	return template.FuncMap{
		// recorderCamName suggests a reostream name for a camera on an NVR
		// channel: its own name ("Track mix" -> track_mix), or ch<N>.
		"recorderCamName": func(name string, ch int) string {
			if n := slugName(name); n != "" {
				return n
			}
			return fmt.Sprintf("ch%d", ch)
		},
		// railEntry, not Camera: Camera carries the password, and the rail
		// needs only a name and a color.
		"fleet": func() []railEntry {
			cams, err := loadFleet(opts.ConfigPath)
			if err != nil {
				return nil
			}
			return railEntries(cams, opts.Status)
		},
		"cameraOf":     cameraOf,
		"dict":         dict,
		"stateLabel":   stateLabel,
		"paletteIndex": func() template.JS { return paletteIndex(opts.ConfigPath) },
		"streamOf":     streamOf,
		"version":      buildVersion,
		"contains":     strings.Contains,
		"lower":        strings.ToLower,
		"inc":          func(n int) int { return n + 1 },
		"split":        strings.Fields,
		"base":         func() string { return "" },
		"at":           func(u string) string { return u },
		"ingress":      func() bool { return false },
	}
}

// New builds the control server. It returns an error rather than panicking
// on a template parse failure so a caller can report it cleanly, rather
// than a process that has not opened a listener yet crashing outright.
func New(opts Options) (*Server, error) {
	funcs := templateFuncs(opts)
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
	srv := &Server{
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
		flashes:        make(map[string]flashEntry),
		done:           make(chan struct{}),
		inFlightProbes: make(map[string]bool),
	}
	// The clip receiver, when the config has one.
	if opts.ConfigPath != "" {
		go srv.syncReceiver()
		go srv.clockScheduler(context.Background())
		go srv.syncRelay()
	}
	// A battery camera's motion, seen from its waking, goes out with the
	// rest of the alerts.
	if h, ok := opts.Supervisor.(motionHooker); ok {
		h.SetMotionHook(relay.batteryMotion)
	}
	return srv, nil
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
//
// A request through Home Assistant's ingress is let through: Home
// Assistant signed the user in before relaying it, and only relays it from
// an address nothing else can connect from (see viaIngress). Asking for a
// second password inside Home Assistant's own page would add a step
// without adding a lock.
func (s *Server) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if viaIngress(r) {
			h.ServeHTTP(w, r)
			return
		}
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
// routePatterns is every pattern Handler registers, in registration
// order, and publicRoutes is the subset deliberately left outside the
// auth wrapper. They exist so a test can ask what the router serves
// rather than compare it against a list somebody has to remember to
// update: a test written to catch a forgotten auth wrapper cannot do
// that while it is itself hand maintained, which is how GET /stream/
// came to be missing from it. See routes_test.go.
//
// Kept beside Handler deliberately. If you add a route there and not
// here, the test below fails and names it.
var routePatterns = []string{
	"GET /claim",
	"POST /claim",
	"GET /login",
	"POST /login",
	"GET /{$}",
	"GET /wall",
	"POST /fleet/order",
	"GET /logs",
	"GET /logs/history",
	"GET /logs/stream",
	"GET /config",
	"POST /config",
	"GET /password",
	"POST /password",
	"POST /logout",
	"GET /cameras",
	"POST /cameras/add",
	"POST /cameras/{name}/remove",
	"GET /cameras/{name}",
	"POST /cameras/{name}/settings",
	"POST /cameras/{name}/rename",
	"POST /cameras/{name}/schedule",
	"POST /cameras/{name}/zone",
	"POST /cameras/{name}/mask",
	"POST /cameras/{name}/rule",
	"POST /cameras/{name}/sound",
	"POST /cameras/{name}/sound/test",
	"POST /cameras/{name}/tracklimit",
	"POST /cameras/{name}/privacymode",
	"GET /cameras/{name}/settings",
	"GET /cameras/{name}/settings/{cat}",
	"GET /settings",
	"GET /settings/about",
	"GET /settings/nvr",
	"GET /settings/events",
	"POST /settings/events",
	"GET /settings/events/recent",
	"POST /settings/nvr",
	"POST /cameras/{name}/netport",
	"POST /cameras/{name}/ptz",
	"POST /cameras/{name}/copy",
	"GET /cameras/{name}/raw",
	"GET /cameras/{name}/recordings",
	"GET /cameras/{name}/recordings/clip",
	"GET /cameras/{name}/recordings/saved",
	"POST /cameras/{name}/clips",
	"POST /cameras/{name}/raw",
	"POST /cameras/{name}/ptz/preset",
	"POST /cameras/{name}/ptz/guard",
	"POST /cameras/{name}/ptz/track",
	"POST /cameras/{name}/ptz/calibrate",
	"POST /cameras/{name}/ptz/guardpos",
	"GET /cameras/{name}/osd",
	"POST /cameras/{name}/floodlight",
	"POST /cameras/{name}/fisheye",
	"GET /cameras/{name}/view",
	"POST /cameras/{name}/stitch",
	"POST /cameras/{name}/encoding",
	"POST /cameras/{name}/siren",
	"GET /cameras/{name}/talk",
	"GET /cameras/{name}/align",
	"GET /cameras/{name}/time",
	"POST /cameras/{name}/time",
	"POST /cameras/{name}/timezone",
	"POST /cameras/{name}/time/sync",
	"POST /cameras/{name}/time/daily",
	"POST /cameras/{name}/time/hourfmt",
	"POST /cameras/{name}/time/datefmt",
	"GET /cameras/{name}/accounts",
	"GET /cameras/{name}/advanced",
	"POST /cameras/{name}/write/{id}",
	"GET /setup",
	"GET /setup/urls",
	"POST /setup/probe",
	"POST /setup/discover",
	"POST /setup/identify",
	"POST /setup/add-found",
	"GET /assets/",
	"GET /stream/",
}

// publicRoutes are the four that must answer without a session: the two
// claim routes, because while an install is unclaimed there is no
// password for the wrapper to check, and the two login routes, because
// that is where the password is entered. Anything else here is a hole.
var publicRoutes = map[string]bool{
	"GET /claim":  true,
	"POST /claim": true,
	"GET /login":  true,
	"POST /login": true,
}

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
	mux.Handle("GET /wall", s.wrap(http.HandlerFunc(s.serveWall)))
	mux.Handle("POST /fleet/order", s.wrap(http.HandlerFunc(s.serveFleetOrder)))
	mux.Handle("GET /settings", s.wrap(http.HandlerFunc(s.serveSettingsIndex)))
	mux.Handle("GET /settings/about", s.wrap(http.HandlerFunc(s.serveAbout)))
	mux.Handle("GET /settings/nvr", s.wrap(http.HandlerFunc(s.serveNVR)))
	mux.Handle("GET /settings/events", s.wrap(http.HandlerFunc(s.serveEventsPage)))
	mux.Handle("POST /settings/events", s.wrap(http.HandlerFunc(s.saveEventsPage)))
	mux.Handle("GET /settings/events/recent", s.wrap(http.HandlerFunc(s.serveRecentEvents)))
	mux.Handle("POST /settings/nvr", s.wrap(http.HandlerFunc(s.serveNVR)))
	mux.Handle("GET /password", s.wrap(http.HandlerFunc(s.servePasswordPage)))
	mux.Handle("POST /password", s.wrap(http.HandlerFunc(s.serveApplyPassword)))
	mux.Handle("POST /logout", s.wrap(http.HandlerFunc(s.serveLogout)))
	mux.Handle("GET /logs", s.wrap(http.HandlerFunc(s.serveLogsPage)))
	mux.Handle("GET /logs/history", s.wrap(http.HandlerFunc(s.serveLogHistory)))
	mux.Handle("GET /logs/stream", s.wrap(http.HandlerFunc(s.serveLogStream)))
	mux.Handle("GET /config", s.wrap(http.HandlerFunc(s.serveConfigPage)))
	mux.Handle("POST /config", s.wrap(http.HandlerFunc(s.saveConfigPage)))
	mux.Handle("GET /cameras", s.wrap(http.HandlerFunc(s.serveCameras)))
	mux.Handle("POST /cameras/add", s.wrap(http.HandlerFunc(s.saveCamera)))
	mux.Handle("POST /cameras/{name}/remove", s.wrap(http.HandlerFunc(s.serveRemoveCamera)))
	mux.Handle("GET /cameras/{name}", s.wrap(http.HandlerFunc(s.serveCamera)))
	mux.Handle("POST /cameras/{name}/settings", s.wrap(http.HandlerFunc(s.serveApplySetting)))
	mux.Handle("POST /cameras/{name}/rename", s.wrap(http.HandlerFunc(s.serveRename)))
	mux.Handle("POST /cameras/{name}/schedule", s.wrap(http.HandlerFunc(s.serveSchedule)))
	mux.Handle("POST /cameras/{name}/zone", s.wrap(http.HandlerFunc(s.serveZone)))
	mux.Handle("POST /cameras/{name}/mask", s.wrap(http.HandlerFunc(s.serveMask)))
	mux.Handle("POST /cameras/{name}/rule", s.wrap(http.HandlerFunc(s.serveRule)))
	mux.Handle("POST /cameras/{name}/sound", s.wrap(http.HandlerFunc(s.serveSound)))
	mux.Handle("POST /cameras/{name}/sound/test", s.wrap(http.HandlerFunc(s.serveSoundTest)))
	mux.Handle("POST /cameras/{name}/tracklimit", s.wrap(http.HandlerFunc(s.serveTrackLimit)))
	mux.Handle("POST /cameras/{name}/privacymode", s.wrap(http.HandlerFunc(s.servePrivacyMode)))
	mux.Handle("GET /cameras/{name}/settings", s.wrap(http.HandlerFunc(s.serveCameraSettingsIndex)))
	mux.Handle("GET /cameras/{name}/settings/{cat}", s.wrap(http.HandlerFunc(s.serveCameraSettings)))
	mux.Handle("POST /cameras/{name}/netport", s.wrap(http.HandlerFunc(s.serveApplyNetPort)))
	mux.Handle("POST /cameras/{name}/ptz", s.wrap(http.HandlerFunc(s.serveApplyPtz)))
	mux.Handle("POST /cameras/{name}/copy", s.wrap(http.HandlerFunc(s.serveCopySection)))
	mux.Handle("GET /cameras/{name}/raw", s.wrap(http.HandlerFunc(s.serveRawValues)))
	mux.Handle("GET /cameras/{name}/recordings", s.wrap(http.HandlerFunc(s.serveRecordings)))
	mux.Handle("GET /cameras/{name}/recordings/clip", s.wrap(http.HandlerFunc(s.serveRecordingClip)))
	mux.Handle("GET /cameras/{name}/recordings/saved", s.wrap(http.HandlerFunc(s.serveSavedClip)))
	mux.Handle("POST /cameras/{name}/clips", s.wrap(http.HandlerFunc(s.serveApplyClips)))
	mux.Handle("POST /cameras/{name}/raw", s.wrap(http.HandlerFunc(s.serveApplyRawValue)))
	mux.Handle("POST /cameras/{name}/ptz/preset", s.wrap(http.HandlerFunc(s.serveApplyPreset)))
	mux.Handle("POST /cameras/{name}/ptz/guard", s.wrap(http.HandlerFunc(s.serveApplyGuard)))
	mux.Handle("POST /cameras/{name}/ptz/track", s.wrap(http.HandlerFunc(s.serveApplyAutoTrack)))
	mux.Handle("POST /cameras/{name}/ptz/calibrate", s.wrap(http.HandlerFunc(s.serveCalibrate)))
	mux.Handle("POST /cameras/{name}/ptz/guardpos", s.wrap(http.HandlerFunc(s.serveGuardPosition)))
	// Read only, and the status page's toggles are the only caller. Writing
	// them still goes through POST /cameras/{name}/settings above: that
	// handler carries the read-modify-write discipline (read the camera's
	// own document, change one field, send it back), and a second write
	// path is exactly how a composed document reaches a camera.
	mux.Handle("GET /cameras/{name}/osd", s.wrap(http.HandlerFunc(s.serveOSD)))
	mux.Handle("POST /cameras/{name}/floodlight", s.wrap(http.HandlerFunc(s.serveApplyFloodlight)))
	mux.Handle("POST /cameras/{name}/fisheye", s.wrap(http.HandlerFunc(s.serveApplyFisheye)))
	mux.Handle("GET /cameras/{name}/view", s.wrap(http.HandlerFunc(s.serveFisheyeView)))
	mux.Handle("POST /cameras/{name}/stitch", s.wrap(http.HandlerFunc(s.serveApplyStitch)))
	mux.Handle("POST /cameras/{name}/encoding", s.wrap(http.HandlerFunc(s.serveApplyEncoding)))
	mux.Handle("POST /cameras/{name}/siren", s.wrap(http.HandlerFunc(s.serveApplySiren)))
	mux.Handle("GET /cameras/{name}/talk", s.wrap(http.HandlerFunc(s.serveTalk)))
	mux.Handle("GET /cameras/{name}/align", s.wrap(http.HandlerFunc(s.serveAlign)))
	mux.Handle("GET /cameras/{name}/time", s.wrap(http.HandlerFunc(s.serveTime)))
	mux.Handle("POST /cameras/{name}/time", s.wrap(http.HandlerFunc(s.serveApplyTime)))
	mux.Handle("POST /cameras/{name}/timezone", s.wrap(http.HandlerFunc(s.serveApplyTimezone)))
	mux.Handle("POST /cameras/{name}/time/sync", s.wrap(http.HandlerFunc(s.serveSyncClock)))
	mux.Handle("POST /cameras/{name}/time/daily", s.wrap(http.HandlerFunc(s.serveDailySync)))
	mux.Handle("POST /cameras/{name}/time/hourfmt", s.wrap(http.HandlerFunc(s.serveHourFormat)))
	mux.Handle("POST /cameras/{name}/time/datefmt", s.wrap(http.HandlerFunc(s.serveDateFormat)))
	// Accounts get a GET route only. No POST, PUT, PATCH or DELETE route
	// exists for /cameras/{name}/accounts anywhere in this package; see
	// accounts.go's top comment for why. Go's ServeMux answers 405 for a
	// method-specific pattern's path with no matching method, which is
	// what makes a request for any of those methods refuse itself without
	// this handler ever having to notice or check.
	mux.Handle("GET /cameras/{name}/accounts", s.wrap(http.HandlerFunc(s.serveAccounts)))
	mux.Handle("GET /cameras/{name}/advanced", s.wrap(http.HandlerFunc(s.serveBlocks)))
	mux.Handle("POST /cameras/{name}/write/{id}", s.wrap(http.HandlerFunc(s.serveWrite)))
	mux.Handle("GET /setup", s.wrap(http.HandlerFunc(s.serveSetup)))
	mux.Handle("GET /setup/urls", s.wrap(http.HandlerFunc(s.serveURLs)))
	mux.Handle("POST /setup/probe", s.wrap(http.HandlerFunc(s.serveProbe)))
	// Never a GET: this opens a TCP connection to every address in a /24,
	// so it is a POST with a body, not something reachable by navigating or
	// prefetching a link. The setup page does fire it on load, but only at
	// the subnets this install can show are the operator's own -- the one
	// its browser is on, and the ones its cameras are already on -- never a
	// range nobody named.
	mux.Handle("POST /setup/discover", s.wrap(http.HandlerFunc(s.serveDiscover)))
	// One camera, re-probed with a password typed into its own field after
	// the credential-free scan could not read it.
	mux.Handle("POST /setup/identify", s.wrap(http.HandlerFunc(s.serveIdentify)))
	mux.Handle("POST /setup/add-found", s.wrap(http.HandlerFunc(s.serveAddFound)))
	// Assets are served WITHOUT auth, deliberately, and it is the one
	// route on this page that is. Behind s.wrap the stylesheet request
	// itself answered 303 /login, so the login screen -- the page an
	// operator sees every day forever after -- rendered unstyled. (The
	// claim screen escaped that only because claimGate exempts /assets/,
	// which is the same exemption arriving one layer too late.)
	//
	// What is under assets/ is the whole argument: a stylesheet, the
	// vendored mpegts.js and its two license files, and a README about
	// where that copy came from. No secret, no camera name, no config,
	// nothing derived from any of them -- they are the same bytes for
	// every install, and they are already in the published image. Any
	// file added there has to hold to that.
	mux.Handle("GET /assets/", http.StripPrefix("/assets/",
		http.HandlerFunc(serveAsset)))
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
		streamSrv := server.New(s.opts.Hubs).AllowWake()
		mux.Handle("GET /stream/", s.wrap(http.StripPrefix("/stream", streamSrv.StreamHandler())))
	}
	return s.withBase(s.claimGate(mux))
}

// render writes one page. data must carry a Title, which layout.html uses.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	s.rend.RenderWith(w, "templates/"+name, data, requestFuncs(r))
}

// requestFuncs are the template functions whose answer depends on the
// request: where the page is mounted, and whether Home Assistant's ingress
// is in front of it. templateFuncs registers each of them too, answering
// for a page at "/" with nothing in front, so every template parses; these
// replace them for one render. See basepath.go.
func requestFuncs(r *http.Request) template.FuncMap {
	base := basePath(r)
	ingress := viaIngress(r)
	return template.FuncMap{
		"base":    func() string { return base },
		"at":      func(u string) string { return at(base, u) },
		"ingress": func() bool { return ingress },
	}
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
	// The URLs a recorder would use, built by setup.go's own recorderURLs
	// from the host this page was reached on, so the status page shows the
	// same URL the setup page pastes out. A config that would not load at
	// all leaves them empty rather than stopping the page: the stream state
	// below comes from memory and is still worth showing.
	var urls RecorderURLs
	if err == nil {
		urls = recorderURLs(cfg, pageHost(r))
	}
	// The overlay switches each card offers come from the curated OSD group
	// itself, not a second list written out in the template: the XPath the
	// page posts and the XPath osd.go reads have to be the same string, and
	// this is what keeps them one.
	var toggles []Field
	if g, ok := osdGroup(); ok {
		for _, f := range g.Fields {
			if f.Kind == "toggle" {
				toggles = append(toggles, f)
			}
		}
	}
	cards := s.statusCards(urls)
	// Flash: the status page's own overlay switches post through the
	// curated-setting handler and name this page as where they came from,
	// so a toggle from here lands back here with its one-line report.
	s.render(w, r, "dashboard.html", struct {
		Title        string
		Cards        []statusCard
		Alerts       []Alert
		HealthyCount int
		Toggles      []Field
		Flash        *Flash
	}{Title: "Status", Cards: cards, Alerts: alerts(cards), HealthyCount: healthyCount(cards), Toggles: toggles, Flash: s.takeFlash(r)})
}

// serveWall is the redesign's Wall: every camera's tile in one dense grid,
// no per-camera chrome. It reuses the dashboard's own card data, so a camera
// is in exactly the state the Fleet shows it.
func (s *Server) serveWall(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if (err == nil && len(cfg.Cameras) == 0) || errors.Is(err, fs.ErrNotExist) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	var urls RecorderURLs
	if err == nil {
		urls = recorderURLs(cfg, pageHost(r))
	}
	s.render(w, r, "wall.html", struct {
		Title string
		Cards []statusCard
	}{Title: "Wall", Cards: s.statusCards(urls)})
}
