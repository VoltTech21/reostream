package camctl

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
// config.Load, not config.LoadRaw: this program dials cameras, so it needs
// passwords resolved from the environment rather than "$NAME" references.
// The operator page's camera form needs the opposite, because it re-encodes
// the config and writes it back, and resolving there would write every
// camera's real password into the file. This program never writes that file.
func (s *Server) fleet() ([]Camera, error) {
	cfg, err := config.Load(s.opts.ConfigPath)
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
	return Camera{}, fmt.Errorf("camctl: no camera named %q in the config", name)
}
