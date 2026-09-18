package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
)

// Camera is one camera this page can talk to.
type Camera struct {
	Name     string
	Address  string
	Username string
	Password string
}

// fleet reads the camera list from reostream's config.
//
// config.Load, not config.LoadRaw: this process dials cameras, so it needs
// camera passwords resolved from the environment rather than left as
// "$NAME" references, and Load is also what resolves [control]'s own
// password to start this same process's control listener -- there is only
// one process now, and it needs both. The config editor uses LoadRaw
// instead, separately, precisely because it re-encodes the config and
// writes it back: resolving there would bake every camera's real password
// into the file in place of the "$NAME" reference that was deliberately
// keeping it out.
func (s *Server) fleet() ([]Camera, error) {
	return loadFleet(s.opts.ConfigPath)
}

// loadFleet is fleet's free-function core, split out so the sidebar's
// "fleet" template function can load the same list without a *Server.
func loadFleet(configPath string) ([]Camera, error) {
	cfg, err := config.Load(configPath)
	// No config file at all is not a failure, it is a fresh install: there
	// is no config until something claims it, and every page rendered
	// before then -- the claim screen first of all -- draws the sidebar,
	// which calls this. Absent means no cameras yet, so an empty list. A
	// config that exists and will not load still errors: that is a broken
	// file, not an empty fleet, and showing it as one would hide it.
	if errors.Is(err, fs.ErrNotExist) {
		return []Camera{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("control: reading the fleet: %w", err)
	}
	out := make([]Camera, 0, len(cfg.Cameras))
	for _, c := range cfg.Cameras {
		out = append(out, Camera{
			Name: c.Name, Address: c.Address,
			Username: c.Username, Password: c.Password,
		})
	}
	return out, nil
}

// byName finds one camera. Handlers take a camera name in the URL rather
// than an address, so a page cannot be pointed at an arbitrary host.
func (s *Server) byName(name string) (Camera, error) {
	cams, err := s.fleet()
	if err != nil {
		return Camera{}, err
	}
	for _, c := range cams {
		if c.Name == name {
			return c, nil
		}
	}
	return Camera{}, fmt.Errorf("control: no camera named %q in the config", name)
}

// Applying one setting to every camera at once
//
// The problem this exists for is the OSD clock on one camera drifting out
// of step with the other seven. Fixing that by hand, one camera page at a
// time, is the chore the "apply to every camera" box on the time page
// removes: it is the same single-camera write (setNTP, setTimeZone in
// time.go), run once per camera, with its own result reported for each.
//
// Eight cameras are eight independent outcomes. Of the 103 messages probed
// across the three models this fleet actually has, 14 differ, and the
// fisheye and the dual lens model account for most of that disagreement.
// So an apply that confirms on six cameras and refuses on two is not a
// failure to collapse into one line, it is the ordinary shape of the
// answer, and applyAll's whole job is to keep that shape intact all the
// way to the page.

// FleetResult is what happened when a fleet apply reached one camera.
//
// Outcome carries the same confirmed/accepted/refused vocabulary
// WriteResult uses everywhere else in this package: a fleet apply is not a
// different kind of write, it is the same write attempted once per camera,
// and it must not grow a second vocabulary to describe the same three
// things.
type FleetResult struct {
	Camera  string
	Outcome string
	Detail  string
}

// fleetApplyTimeout bounds one camera's turn in a fleet apply.
//
// It is given fresh to every camera applyAll tries, never shared off one
// overall deadline: a camera that is unreachable, powered off, or mid
// reboot must be free to burn its own timeout without leaving less time for
// the cameras behind it in the list.
//
// This is deliberately its own number, not probeTimeout. probeTimeout
// bounds a sweep of around a hundred reads with redials; a fleet apply does
// one CGI round trip per camera (setNTP and setTimeZone each run at most a
// dial, a read, a write, and a read-back: up to four calls, each bounded by
// cgi.Client's own 20 second http.Client timeout, so at most about 80
// seconds in the worst case). applyAll runs the fleet serially, so this
// number is multiplied by the fleet size on the bad day every camera is
// down: at 3 minutes, an eight-camera fleet that is entirely unreachable
// would leave the page looking hung for 24 minutes. 90 seconds covers the
// worst-case four-call chain with headroom without multiplying into
// something an operator would give up on and kill.
//
// cgi.Client.Get and Set both take ctx and hand it to the underlying
// *http.Request, so a camCtx that expires mid-call actually aborts that
// call rather than only being checked between calls: this is a real
// ceiling on the read/write/read-back chain, not merely a number
// multiplied out in a comment. See cgi.Client.do and
// TestGetHonoursContextCancellation in internal/cgi for the mechanism and
// its regression test.
//
// The one call this ceiling still does not reach into is the initial
// login s.cgiDial's cgi.Dial branch performs: that dial carries only its
// own 20 second http.Client timeout, not camCtx, because Options.CGIDial's
// test-double signature has no ctx parameter for a real dial to thread it
// through. In practice this does not widen the worst case recorded above,
// since 20 seconds is already inside the budget the four-call chain
// assumes, but it means camCtx's cancellation specifically is not what
// bounds that one call.
const fleetApplyTimeout = 90 * time.Second

// applyAll runs apply against every configured camera and returns one
// result per camera, in fleet order. It never stops at the first failure,
// and it never skips a camera because an earlier one failed: a fleet apply
// that stopped at the first dead camera would not be a fleet tool, it would
// be a loop with delusions of one.
//
// Each camera gets its own context, derived from ctx but carrying its own
// fresh fleetApplyTimeout, so one camera's bound cannot be eaten by another
// camera's turn ahead of it in the list.
func (s *Server) applyAll(ctx context.Context, apply func(context.Context, Camera) (WriteResult, error)) []FleetResult {
	cams, err := s.fleet()
	if err != nil {
		return []FleetResult{{Outcome: "refused", Detail: fmt.Sprintf("could not read the fleet: %v", err)}}
	}

	results := make([]FleetResult, 0, len(cams))
	for _, cam := range cams {
		camCtx, cancel := context.WithTimeout(ctx, fleetApplyTimeout)
		result, err := apply(camCtx, cam)
		cancel()
		if err != nil {
			results = append(results, FleetResult{Camera: cam.Name, Outcome: "refused", Detail: err.Error()})
			continue
		}
		results = append(results, FleetResult{Camera: cam.Name, Outcome: result.Outcome, Detail: result.Detail})
	}
	return results
}

// What may be pushed to every camera at once is gated by the handlers that
// call applyAll, not by a lookup table: serveApplyTime and
// serveApplyTimezone in time.go are the only two callers, each hardcoded to
// the one setter (setNTP, setTimeZone) it applies. settings.go's groups()
// and the rest of the camera page expose far more editable fields than
// that, and none of them gained the ability to be written to eight cameras
// in one request just by having a form control. The next field that wants
// one needs its own handler, on purpose, the way these two have theirs.
