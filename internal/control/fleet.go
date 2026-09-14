package control

import (
	"fmt"

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
// config.LoadForDialing, not config.Load and not config.LoadRaw: this
// program dials cameras, so it needs camera passwords resolved from the
// environment rather than "$NAME" references. But it never dials
// reostream's own control listener, so unlike the streaming daemon it has
// no business requiring [control].password to resolve too, and in the real
// deployment that variable lives in reostream's container, not this one's.
// config.Load requiring it made every page 500 on startup; see
// LoadForDialing's own comment. The operator page's camera form needs a
// third thing again, LoadRaw, because it re-encodes the config and writes
// it back, and resolving there would write every camera's real password
// into the file. This program never writes that file.
func (s *CameraServer) fleet() ([]Camera, error) {
	return loadFleet(s.opts.ConfigPath)
}

// loadFleet is fleet's free-function core, split out so the sidebar's
// "fleet" template function can load the same list without a *CameraServer.
func loadFleet(configPath string) ([]Camera, error) {
	cfg, err := config.LoadForDialing(configPath)
	if err != nil {
		return nil, fmt.Errorf("camctl: reading the fleet: %w", err)
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
func (s *CameraServer) byName(name string) (Camera, error) {
	cams, err := s.fleet()
	if err != nil {
		return Camera{}, err
	}
	for _, c := range cams {
		if c.Name == name {
			return c, nil
		}
	}
	return Camera{}, fmt.Errorf("camctl: no camera named %q in the config", name)
}
