// camserver.go builds CameraServer: what a camera is, what it will tell you
// about itself, and what it will let you change.
//
// This used to run as its own process on its own port, not as more routes
// on the streaming daemon's operator page, because the split was by write
// target: control.go's own server edits reostream's own config and never
// writes to a camera, while this writes to cameras, is much larger, and a
// fault in it must never be able to take video down. That split in
// behaviour still holds; only the package boundary that used to enforce it
// is gone.
package control

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"reflect"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/cgi"
	"github.com/VoltTech21/reostream/internal/webui"
)

// Listed explicitly, not templates/*.html: this directory also holds
// Server's own operator-page templates (see control.go's templateFS), and a
// wildcard here would pull layout.html's "layout" definition into this
// server's base template set alongside cameralayout.html's, with whichever
// parses last silently winning. Naming exactly this server's own files
// keeps the two template sets from ever touching.
//
//go:embed templates/accounts.html templates/blocks.html templates/camera.html templates/cameralayout.html templates/cameralogin.html templates/fleet.html templates/fleetapply.html templates/result.html templates/settings.html templates/time.html
var cameraTemplateFS embed.FS

//go:embed assets
var cameraAssetFS embed.FS

// cameraAssetSub drops the "assets" prefix so the URL and the file path
// match.
var cameraAssetSub, _ = fs.Sub(cameraAssetFS, "assets")

// cameraOf finds a field named Camera on whatever page data v is, and
// returns it as a *Camera, or nil when the page has no such field. The
// sidebar template uses this to decide whether it is looking at a
// camera-specific page (blocks, settings, time, accounts, the camera
// overview) or a fleet-wide one (the fleet list, fleet apply, login):
// those pages carry no Camera at all, and a plain {{.Camera}} in the
// shared layout would fail to execute on them.
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

// CameraOptions is everything the camera control server needs.
type CameraOptions struct {
	Password        string
	AllowNoPassword bool
	ConfigPath      string

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

// CameraServer serves the camera control page.
type CameraServer struct {
	opts     CameraOptions
	rend     *webui.Renderer
	auth     webui.Auth
	sessions *webui.SessionStore
}

// NewCameraServer builds a camera control server. It returns an error
// rather than panicking on a template parse failure so a caller can report
// it cleanly, rather than this package crashing a process that has not
// opened a listener yet.
func NewCameraServer(opts CameraOptions) (*CameraServer, error) {
	// The sidebar needs the fleet on every page, not just serveFleet's own,
	// so it is a template function bound to this daemon's config path
	// rather than a field every page struct would otherwise have to carry.
	// A page that cannot read the config renders its sidebar without a
	// fleet list rather than failing the whole render.
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
	rend, err := webui.NewRenderer(cameraTemplateFS, "templates/*.html", funcs)
	if err != nil {
		return nil, err
	}
	sessions := webui.NewSessionStore()
	return &CameraServer{
		opts: opts,
		rend: rend,
		auth: webui.Auth{
			Store:           sessions,
			Password:        opts.Password,
			AllowNoPassword: opts.AllowNoPassword,
			LoginPath:       "/login",
			// Distinct from Server's own cookie name: the two pages still
			// run their own independent SessionStores, and a shared name
			// would mean logging into one silently logs the other out. See
			// webui.Auth's own comment.
			CookieName: "reostream_camctl_session",
		},
		sessions: sessions,
	}, nil
}

// Handler returns the routes for this page. Everything but the login routes
// runs behind auth.Wrap.
func (s *CameraServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.serveLoginForm)
	mux.HandleFunc("POST /login", s.serveLogin)
	mux.Handle("GET /{$}", s.auth.Wrap(http.HandlerFunc(s.serveFleet)))
	mux.Handle("GET /fleet/apply", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyForm)))
	mux.Handle("POST /fleet/apply/ntp", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyNTP)))
	mux.Handle("POST /fleet/apply/timezone", s.auth.Wrap(http.HandlerFunc(s.serveFleetApplyTimezone)))
	mux.Handle("GET /camera/{name}", s.auth.Wrap(http.HandlerFunc(s.serveCamera)))
	mux.Handle("GET /camera/{name}/blocks", s.auth.Wrap(http.HandlerFunc(s.serveBlocks)))
	mux.Handle("GET /camera/{name}/settings", s.auth.Wrap(http.HandlerFunc(s.serveSettings)))
	mux.Handle("POST /camera/{name}/settings", s.auth.Wrap(http.HandlerFunc(s.serveApplySetting)))
	mux.Handle("POST /camera/{name}/floodlight", s.auth.Wrap(http.HandlerFunc(s.serveApplyFloodlight)))
	mux.Handle("GET /camera/{name}/time", s.auth.Wrap(http.HandlerFunc(s.serveTime)))
	mux.Handle("POST /camera/{name}/time", s.auth.Wrap(http.HandlerFunc(s.serveApplyTime)))
	// Accounts get a GET route only. No POST, PUT, PATCH or DELETE route
	// exists for /camera/{name}/accounts anywhere in this package; see
	// accounts.go's top comment for why. Go's ServeMux answers 405 for a
	// method-specific pattern's path with no matching method, which is
	// what makes a request for any of those methods refuse itself without
	// this handler ever having to notice or check.
	mux.Handle("GET /camera/{name}/accounts", s.auth.Wrap(http.HandlerFunc(s.serveAccounts)))
	mux.Handle("POST /camera/{name}/write/{id}", s.auth.Wrap(http.HandlerFunc(s.serveWrite)))
	mux.Handle("GET /assets/", s.auth.Wrap(http.StripPrefix("/assets/",
		http.FileServer(http.FS(cameraAssetSub)))))
	return mux
}

// render writes one page. data must carry a Title, which cameralayout.html
// uses.
func (s *CameraServer) render(w http.ResponseWriter, name string, data any) {
	s.rend.Render(w, "templates/"+name, data)
}
