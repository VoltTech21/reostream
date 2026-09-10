package control

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"

	"github.com/BurntSushi/toml"
	"github.com/VoltTech21/reostream/internal/config"
)

// passwordUnchanged is what the form submits when the operator did not
// touch the password field. The page never renders an existing password, so
// it needs a value that means "leave it" and cannot collide with a real
// one; a literal NUL cannot appear in a form field a person typed.
const passwordUnchanged = "\u0000unchanged"

// applyCameraForm folds one camera's form submission into cfg, adding it if
// its name is new and replacing it if not.
func applyCameraForm(cfg *config.Config, form url.Values) error {
	cam := config.Camera{
		Name:     form.Get("name"),
		Address:  form.Get("address"),
		Username: form.Get("username"),
		Password: form.Get("password"),
		Streams:  form["streams"],
		RTSP:     form["rtsp"],
	}

	for i, existing := range cfg.Cameras {
		if existing.Name != cam.Name {
			continue
		}
		if cam.Password == passwordUnchanged {
			cam.Password = existing.Password
		}
		cfg.Cameras[i] = cam
		return validateOne(cfg)
	}

	if cam.Password == passwordUnchanged {
		cam.Password = ""
	}
	cfg.Cameras = append(cfg.Cameras, cam)
	return validateOne(cfg)
}

// validateOne runs the daemon's own validation over the whole config, so a
// form submission is held to exactly the rules startup enforces.
func validateOne(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// formCamera is a config.Camera with the helpers the template needs.
type formCamera struct {
	config.Camera
}

// HasStream reports whether this camera pulls the named stream, so the
// template can tick the right boxes.
func (c formCamera) HasStream(name string) bool {
	for _, s := range c.Streams {
		if s == name {
			return true
		}
	}
	return false
}

type camerasPage struct {
	Title      string
	Cameras    []formCamera
	AllStreams []string
	Unchanged  string
	Error      string
	ReloadNote string
}

func (s *Server) camerasPage() camerasPage {
	page := camerasPage{
		Title:      "Cameras",
		AllStreams: []string{"main", "sub", "extern"},
		Unchanged:  passwordUnchanged,
	}
	// LoadRaw, not Load: this page renders the config's own text back into
	// a form, and Load would resolve any "$NAME" password reference into
	// the real secret before it ever reaches formCamera.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		return page
	}
	for _, cam := range cfg.Cameras {
		page.Cameras = append(page.Cameras, formCamera{Camera: cam})
	}
	return page
}

func (s *Server) serveCameras(w http.ResponseWriter, r *http.Request) {
	s.render(w, "cameras.html", s.camerasPage())
}

// saveCamera folds one camera into the config and then goes through the
// same write and apply path the raw editor uses. Re-encoding to TOML and
// handing the text to writeAndApply is what keeps that one path: a second
// writer would be a second place for the backup, the validation and the
// reload to drift out of agreement.
func (s *Server) saveCamera(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// LoadRaw, not Load: cfg here is re-encoded and written back below, and
	// Load would turn every camera's "$NAME" password reference into that
	// camera's real, resolved secret before it ever reached the encoder,
	// writing every live credential into the config file in plaintext for
	// the sake of editing one unrelated camera.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		page := s.camerasPage()
		page.Error = err.Error()
		s.render(w, "cameras.html", page)
		return
	}

	if err := applyCameraForm(cfg, r.Form); err != nil {
		page := s.camerasPage()
		page.Error = err.Error()
		s.render(w, "cameras.html", page)
		return
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		page := s.camerasPage()
		page.Error = err.Error()
		s.render(w, "cameras.html", page)
		return
	}

	note, err := s.writeAndApply(buf.String())
	page := s.camerasPage()
	if err != nil {
		page.Error = err.Error()
	} else {
		page.ReloadNote = note
	}
	s.render(w, "cameras.html", page)
}
