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

	// Channel picks one camera behind a device that serves several, a
	// Reolink Home Hub or NVR, where Address is the hub's and each paired
	// camera is a numbered channel from 0. A camera on its own is always
	// channel 0, which is also what leaving it out means.
	Channel int

	// UID marks a battery camera, reached over UDP by this id instead of
	// TCP port 9000.
	UID string

	// RTSP names which of this camera's streams to serve over RTSP. Absent
	// means none, and every stream still reaches the HTTP output either way.
	RTSP []string

	// HoldPicture keeps this camera's RTSP streams showing the last picture
	// while the camera sends no video (privacy mode, a dropout), instead of
	// going silent, so a recorder's ffmpeg is not restarted over and over
	// (#13). Off by default: a frozen picture also hides a real outage.
	HoldPicture bool `toml:"hold_picture"`

	// MotionTopic is an MQTT topic, on the [events] broker, where another
	// tool reports this camera's motion as ON / OFF: reoevent, which hears
	// a battery camera's own motion reports on the router. reostream
	// relays it like the camera's own alerts and, for a battery camera,
	// wakes it for the motion so its presence stream carries it live.
	MotionTopic string `toml:"motion_topic"`

	// AlwaysOn keeps a battery camera (one with a UID) streaming around the
	// clock, like a wired camera, instead of sleeping between viewers and
	// motion. Only for a battery camera on constant power: on its battery
	// it drains in days. Off by default.
	AlwaysOn bool `toml:"always_on"`

	// Rotate turns this camera's picture for anything that decodes it:
	// 90, 180 or 270 degrees anticlockwise, written into the H.265 stream
	// as a display orientation note rather than re-encoded. Unset, the
	// camera's own word decides: the 16 MP dual-lens boards encode their
	// full resolution sideways and say so at login (needRotate). Set it to
	// 0 to leave a camera's picture alone whatever it says.
	Rotate *int `toml:"rotate"`
}

// RTSPConfig configures the RTSP listener. Its absence turns RTSP off
// entirely, which is the default: HTTP MPEG-TS is what this daemon has
// always served, and RTSP is opt-in for consumers that cannot take it.
type RTSPConfig struct {
	Listen string

	// Username and Password, when set, are required of every reader of
	// the video outputs: RTSP (Basic or Digest) and the HTTP streaming
	// port (Basic). Unset is open, which is what every setup before them
	// was, so an upgrade does not lock a recorder out. They live here
	// rather than in a section of their own because a recorder holds one
	// login for reostream, whichever output it reads. Either may be
	// "$NAME", read from the environment like a camera's password.
	Username string
	Password string
}

// Credentials reports whether the video outputs require a login, and which.
func (r *RTSPConfig) Credentials() (user, pass string, ok bool) {
	if r == nil || r.Username == "" {
		return "", "", false
	}
	return r.Username, r.Password, true
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

	// BasePath serves the page under a path prefix, such as "/reostream",
	// for a reverse proxy that puts it there. Empty is the root.
	BasePath string `toml:"base_path"`
}

// Config is the top level shape of the TOML file.
type Config struct {
	Listen  string
	RTSP    *RTSPConfig    `toml:"rtsp"`
	Control *ControlConfig `toml:"control"`
	FTP     *FTPConfig     `toml:"ftp"`
	Events  *EventsConfig  `toml:"events"`
	Clock   *ClockConfig   `toml:"clock"`
	Cameras []Camera       `toml:"camera"`
}

// ClockConfig keeps the cameras' clocks right.
type ClockConfig struct {
	// DailySync is the time of day, "HH:MM" on this machine's clock, at
	// which every wired camera's clock is set from this machine's. Empty
	// is off.
	DailySync string `toml:"daily_sync"`
}

// EventsConfig relays each camera's own detections (motion, person,
// vehicle, animal) as they happen. Either destination may be left empty.
type EventsConfig struct {
	// MQTT is the broker, host:port; MQTTUser and MQTTPassword its login.
	MQTT         string `toml:"mqtt"`
	MQTTUser     string `toml:"mqtt_user"`
	MQTTPassword string `toml:"mqtt_password"`
	// Prefix starts every topic; "reostream" when empty.
	Prefix string
	// Frigate is Frigate's API, e.g. "http://192.0.2.5:5000": an object the
	// camera sees becomes a Frigate event on the camera of the same name.
	Frigate string
	// FrigateMotion also sends plain motion to Frigate, which finds motion
	// itself, so it is off unless asked for.
	FrigateMotion bool `toml:"frigate_motion"`
	// HADiscovery announces each camera to Home Assistant over MQTT
	// discovery, so it appears there as a device with its sensors. It
	// needs MQTT set.
	HADiscovery bool `toml:"ha_discovery"`
}

// FTPConfig is the clip receiver: cameras upload each recording here over
// FTP, logging in with their own camera name and this password. Absent, no
// receiver runs.
type FTPConfig struct {
	// Listen is the control port, ":2121" when empty.
	Listen string
	// Passive is the data port range, "2122-2131" when empty.
	Passive string
	// Host is the address cameras are told to send data to; set it when
	// reostream runs behind NAT (Docker's bridge network).
	Host string
	// Dir holds the clips, one folder per camera; "clips" next to the
	// config file when empty.
	Dir string
	// Password is what every camera logs in with.
	Password string
	// KeepDays deletes clips older than this; 0 keeps them forever.
	KeepDays int `toml:"keep_days"`
}

// NormalizeAddr applies the same default-port rule baichuan.Dial does, so
// "192.0.2.50" and "192.0.2.50:9000" compare equal as the same camera.
//
// Exported so a caller outside this package that also needs to recognize
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
	// The TrackMix's zoom lens, read from the camera's own RTSP server:
	// it is not available over Baichuan. See internal/stream/native.go.
	"zoom": true,
	// The zoom lens's sub stream (896x512 on a TrackMix).
	"zoom_sub": true,
}

// Load reads and validates the config file at path. A camera address,
// username or password, the control password, or the [rtsp] username or
// password, that starts with "$" is resolved from the named environment
// variable rather than stored in the file; an unset variable is an error
// rather than an empty password, because a camera that silently never
// authenticates is much harder to notice than a process that refuses to
// start.
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

	// Address, username and password all take "$NAME", so a camera can be
	// configured entirely from the environment (a Kubernetes Secret, say)
	// with nothing site specific left in the file itself.
	for i, cam := range cfg.Cameras {
		for _, f := range []struct {
			key string
			v   *string
		}{
			{"address", &cfg.Cameras[i].Address},
			{"username", &cfg.Cameras[i].Username},
			{"password", &cfg.Cameras[i].Password},
		} {
			if !strings.HasPrefix(*f.v, "$") {
				continue
			}
			name := strings.TrimPrefix(*f.v, "$")
			val, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("config: %s: camera %q: %s: environment variable %s is not set", path, cam.Name, f.key, name)
			}
			*f.v = val
		}
	}

	if cfg.RTSP != nil {
		for _, f := range []struct {
			key string
			v   *string
		}{
			{"username", &cfg.RTSP.Username},
			{"password", &cfg.RTSP.Password},
		} {
			if !strings.HasPrefix(*f.v, "$") {
				continue
			}
			name := strings.TrimPrefix(*f.v, "$")
			val, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("config: %s: rtsp: %s: environment variable %s is not set", path, f.key, name)
			}
			*f.v = val
		}
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
// alone: names and addresses present, streams recognized, and no two
// cameras or streams that would fight over the same connection. A camera
// permits exactly one connection per stream, so a duplicate here is not a
// harmless redundancy, it is two runners that will fight for one session
// and the loser blocks the winner until the camera times the session out.
func (c *Config) Validate() error {
	if c.Control != nil && c.Control.Listen != "" &&
		c.Control.Password == "" && !c.Control.AllowNoPassword {
		return fmt.Errorf("control: listen is set with no password; set one or set allow_no_password = true")
	}

	if r := c.RTSP; r != nil && (r.Username != "" || r.Password != "") {
		// A password with no username would read as "protected" in the
		// file and be open on the wire: gortsplib refuses an empty
		// username, and so would the Basic check on the HTTP port.
		if r.Username == "" || r.Password == "" {
			return fmt.Errorf("rtsp: set both username and password, or neither")
		}
		// Basic authentication sends "user:password" and splits it at the
		// first colon, so a colon in the username can never match.
		if strings.Contains(r.Username, ":") {
			return fmt.Errorf("rtsp: username cannot contain a colon")
		}
	}

	// An empty Cameras list is deliberately not rejected here: the loop
	// below simply does not run, and Validate returns nil. A fresh install
	// has no cameras configured yet and must still start, serve its setup
	// page, and let somebody add the first one -- see cmd/reostream's
	// first-run handling. Do not add a len(c.Cameras) == 0 check back as a
	// tidiness fix; it would make that first run impossible.
	seenNames := make(map[string]bool, len(c.Cameras))
	// seenAddrStreams tracks, per normalized address, which stream names
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
		if cam.MotionTopic != "" && (c.Events == nil || c.Events.MQTT == "") {
			return fmt.Errorf("camera %q: motion_topic needs an [events] section with mqtt", cam.Name)
		}
		if r := cam.Rotate; r != nil && *r != 0 && *r != 90 && *r != 180 && *r != 270 {
			return fmt.Errorf("camera %q: rotate is %d; use 0, 90, 180 or 270", cam.Name, *r)
		}
		if cam.AlwaysOn && cam.UID == "" {
			return fmt.Errorf("camera %q: always_on is for a battery camera, one with a uid", cam.Name)
		}
		if cam.Channel < 0 || cam.Channel > 63 {
			return fmt.Errorf("camera %q: channel %d is out of range (0 to 63)", cam.Name, cam.Channel)
		}
		if seenNames[cam.Name] {
			return fmt.Errorf("camera name %q is used more than once", cam.Name)
		}
		seenNames[cam.Name] = true

		// A battery camera may list no streams: reostream then only reaches
		// it on demand (settings, recordings) and never holds it awake.
		if len(cam.Streams) == 0 && cam.UID == "" {
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

		// A hub serves each of its cameras as a channel on one address, so
		// it is address and channel together that name one camera.
		naddr := NormalizeAddr(cam.Address)
		if cam.Channel != 0 {
			naddr = fmt.Sprintf("%s#%d", naddr, cam.Channel)
		}
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
