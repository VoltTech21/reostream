package control

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/VoltTech21/reostream/internal/config"
)

// saveConfig validates text and, only if it is a config the daemon would
// boot on, replaces the file at path.
//
// Validation runs the real loader, not a second implementation of it, so
// the browser rejects exactly what startup would reject, including the
// unknown key check that exists to stop a typo becoming a silent 404 at
// three in the morning.
func saveConfig(path, text string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".reostream-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if _, err := config.Load(tmp.Name()); err != nil {
		// Reported verbatim: the loader's messages name the camera and the
		// key, and a rewritten version of them would be less useful.
		return fmt.Errorf("%w", err)
	}

	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("could not write backup: %w", err)
		}
	}

	// Rename, so a crash mid-write cannot leave a half written config that
	// the next boot refuses.
	return os.Rename(tmp.Name(), path)
}

// loadRawConfig returns the config file as text for the editor.
func loadRawConfig(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type configPage struct {
	Title      string
	Text       string
	Error      string
	Saved      bool
	ReloadNote string
}

func (s *Server) serveConfigPage(w http.ResponseWriter, r *http.Request) {
	text, err := loadRawConfig(s.opts.ConfigPath)
	page := configPage{Title: "Config", Text: text}
	if err != nil {
		page.Error = err.Error()
	}
	s.render(w, "config.html", page)
}

func (s *Server) saveConfigPage(w http.ResponseWriter, r *http.Request) {
	text := r.FormValue("toml")
	if err := saveConfig(s.opts.ConfigPath, text); err != nil {
		s.render(w, "config.html", configPage{
			Title: "Config", Text: text, Error: err.Error(),
		})
		return
	}
	s.render(w, "config.html", configPage{
		Title: "Config", Text: text, Saved: true,
		ReloadNote: "Not applied yet.",
	})
}
