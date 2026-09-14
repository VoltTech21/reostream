// Package config loads the TOML file that lists the cameras reostream
// serves. Unknown keys are a boot-time error rather than a warning: a typo
// in a camera's fields should fail loudly at startup, not silently drop the
// field and serve 404s at three in the morning.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Camera names one Reolink camera and the streams to pull from it.
type Camera struct {
	Name     string
	Address  string
	Username string
	Password string
	Streams  []string

	// RTSP names which of this camera's streams to serve over RTSP. Absent
	// means none, and every stream still reaches the HTTP output either way.
	RTSP []string
}

// RTSPConfig configures the RTSP listener. Its absence turns RTSP off
// entirely, which is the default: HTTP MPEG-TS is what this daemon has
// always served, and RTSP is opt-in for consumers that cannot take it.
type RTSPConfig struct {
	Listen string
}

// ControlConfig configures the operator page's listener. Its absence turns
// the page off entirely, which is the default: a daemon that serves video
// should not start serving a credential store because it was upgraded.
type ControlConfig struct {
	Listen   string
	Password string

	// AllowNoPassword runs the page with no authentication at all. It is a
	// separate opt-in rather than the meaning of an empty password because
	// an empty password is much more often a mistake, and this page can
	// read and write camera credentials.
	AllowNoPassword bool `toml:"allow_no_password"`
}

// Config is the top level shape of the TOML file.
type Config struct {
	Listen  string
	RTSP    *RTSPConfig `toml:"rtsp"`
	Control *ControlConfig `toml:"control"`
	Cameras []Camera    `toml:"camera"`
}

// NormalizeAddr applies the same default-port rule baichuan.Dial does, so
// "192.0.2.50" and "192.0.2.50:9000" compare equal as the same camera.
//
// Exported so a caller outside this package that also needs to recognise
// "is this the same camera as one already configured" -- the setup page's
// probe guard is the current one -- compares addresses the same way
// Validate does below, rather than keeping a second implementation that can
// quietly drift from this one.
func NormalizeAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "9000")
	}
	return addr
}

// validStreamNames mirrors the three independent connections a Reolink
// camera exposes; internal/stream.streamKind maps these same three strings
// to Baichuan's wire values.
var validStreamNames = map[string]bool{
	"main":   true,
	"sub":    true,
	"extern": true,
}

// Load reads and validates the config file at path. A password value that
// starts with "$" is resolved from the named environment variable rather
// than stored in the file; an unset variable is an error rather than an
// empty password, because a camera that silently never authenticates is
// much harder to notice than a process that refuses to start.
//
// This resolves [control]'s password too, which is right for the one
// process that both dials cameras and listens on the control port.
func Load(path string) (*Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config: %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	for i, cam := range cfg.Cameras {
		if !strings.HasPrefix(cam.Password, "$") {
			continue
		}
		name := strings.TrimPrefix(cam.Password, "$")
		val, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("config: %s: camera %q: environment variable %s is not set", path, cam.Name, name)
		}
		cfg.Cameras[i].Password = val
	}

	if cfg.Control != nil && strings.HasPrefix(cfg.Control.Password, "$") {
		name := strings.TrimPrefix(cfg.Control.Password, "$")
		val, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("config: %s: control: environment variable %s is not set", path, name)
		}
		cfg.Control.Password = val
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	return &cfg, nil
}

// LoadRaw decodes the TOML file at path with the same unknown-key
// rejection Load applies, but performs no environment resolution: a
// camera's Password field comes back exactly as written, including an
// unresolved "$NAME" reference, and no environment variable needs to be
// set for this to succeed.
//
// This exists for callers that read a config only to write it back, such
// as the control page's camera form. Editing the file's text should not
// require the file's secrets to pass through the process first, and
// resolving them here would mean writing a real password back to disk in
// place of the "$NAME" reference that was deliberately keeping it out of
// the file.
//
// Never use the Config this returns to actually connect to a camera; its
// passwords may not be passwords at all.
func LoadRaw(path string) (*Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config: %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	return &cfg, nil
}

// Validate checks the constraints Load cannot express through decoding
// alone: names and addresses present, streams recognised, and no two
// cameras or streams that would fight over the same connection. A camera
// permits exactly one connection per stream, so a duplicate here is not a
// harmless redundancy, it is two runners that will fight for one session
// and the loser blocks the winner until the camera times the session out.
func (c *Config) Validate() error {
	if c.Control != nil && c.Control.Listen != "" &&
		c.Control.Password == "" && !c.Control.AllowNoPassword {
		return fmt.Errorf("control: listen is set with no password; set one or set allow_no_password = true")
	}

	// An empty Cameras list is deliberately not rejected here: the loop
	// below simply does not run, and Validate returns nil. A fresh install
	// has no cameras configured yet and must still start, serve its setup
	// page, and let somebody add the first one -- see cmd/reostream's
	// first-run handling. Do not add a len(c.Cameras) == 0 check back as a
	// tidiness fix; it would make that first run impossible.
	seenNames := make(map[string]bool, len(c.Cameras))
	// seenAddrStreams tracks, per normalised address, which stream names
	// are already claimed by which camera, so two differently named
	// cameras that both point at one physical camera and both ask for its
	// main stream are caught here. Two cameras sharing an address but
	// asking for disjoint streams are deliberately NOT rejected: a real
	// camera's main, sub and extern are already independent Baichuan
	// connections (see the supervisor package's doc comment), so splitting
	// them across two config entries is no different from one entry
	// listing all three, and forbidding it would only punish an unusual
	// but harmless way of writing the file.
	seenAddrStreams := make(map[string]map[string]string)
	for _, cam := range c.Cameras {
		if cam.Name == "" {
			return fmt.Errorf("camera has no name")
		}
		if cam.Address == "" {
			return fmt.Errorf("camera %q has no address", cam.Name)
		}
		if seenNames[cam.Name] {
			return fmt.Errorf("camera name %q is used more than once", cam.Name)
		}
		seenNames[cam.Name] = true

		if len(cam.Streams) == 0 {
			return fmt.Errorf("camera %q has no streams", cam.Name)
		}
		seenStreams := make(map[string]bool, len(cam.Streams))
		for _, s := range cam.Streams {
			if !validStreamNames[s] {
				return fmt.Errorf("camera %q: unknown stream %q", cam.Name, s)
			}
			if seenStreams[s] {
				return fmt.Errorf("camera %q: stream %q is listed more than once", cam.Name, s)
			}
			seenStreams[s] = true
		}

		naddr := NormalizeAddr(cam.Address)
		for _, s := range cam.Streams {
			if owner, taken := seenAddrStreams[naddr][s]; taken {
				return fmt.Errorf("camera %q and camera %q share address %q on stream %q: "+
					"a camera allows only one connection per stream, and a second runner "+
					"against the same one gets a session the camera refuses",
					owner, cam.Name, cam.Address, s)
			}
		}
		if seenAddrStreams[naddr] == nil {
			seenAddrStreams[naddr] = make(map[string]string, len(cam.Streams))
		}
		for _, s := range cam.Streams {
			seenAddrStreams[naddr][s] = cam.Name
		}

		for _, want := range cam.RTSP {
			if !validStreamNames[want] {
				return fmt.Errorf("camera %q: unknown rtsp stream %q", cam.Name, want)
			}
			if !seenStreams[want] {
				// Serving a stream the camera is not pulling would publish a
				// URL that never carries a frame, and a URL that is silent
				// rather than absent is much harder to diagnose.
				return fmt.Errorf("camera %q: rtsp stream %q is not in streams", cam.Name, want)
			}
		}
		if c.RTSP == nil && len(cam.RTSP) > 0 {
			return fmt.Errorf("camera %q sets rtsp but there is no [rtsp] section", cam.Name)
		}
	}
	return nil
}
