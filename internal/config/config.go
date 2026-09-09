// Package config loads the TOML file that lists the cameras reostream
// serves. Unknown keys are a boot-time error rather than a warning: a typo
// in a camera's fields should fail loudly at startup, not silently drop the
// field and serve 404s at three in the morning.
package config

import (
	"fmt"
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

// Config is the top level shape of the TOML file.
type Config struct {
	Listen  string
	RTSP    *RTSPConfig `toml:"rtsp"`
	Cameras []Camera    `toml:"camera"`
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

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
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
	seenNames := make(map[string]bool, len(c.Cameras))
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
