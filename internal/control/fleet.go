package control

import (
	"errors"
	"fmt"
	"io/fs"

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
