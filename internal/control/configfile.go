package control

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/VoltTech21/reostream/internal/config"
)

// renameConfig is os.Rename, kept as a variable so a test can force the
// bind-mount fallback path below without needing a real single-file bind
// mount, which cannot be created in a test.
var renameConfig = os.Rename

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
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if _, err := config.Load(tmpPath); err != nil {
		// config.Load embeds the path it was given in its message, and
		// here that is the temp file's random name, not anything the
		// operator recognizes. Swap in the real path so the message they
		// see matches the file they are editing; the rest of the loader's
		// wording, which names the camera and the key, is left intact.
		return errors.New(strings.Replace(err.Error(), tmpPath, path, 1))
	}

	// Preserve whatever mode the target already has rather than letting a
	// rename silently replace it with os.CreateTemp's 0600. A target that
	// does not exist yet gets 0600, the right default for a file that
	// holds camera credentials.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}

	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("could not write backup: %w", err)
		}
	}

	// Rename, so a crash mid-write cannot leave a half written config that
	// the next boot refuses. This is the fast, atomic path and is correct
	// wherever the filesystem allows it.
	if err := renameConfig(tmpPath, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) && !errors.Is(err, syscall.EXDEV) {
			return err
		}
		// Docker's single-file bind mount (-v host/config.toml:/config.toml,
		// which is how this daemon's config actually reaches it in the
		// only environment this feature ships to) attaches the mount to
		// the target's inode. Rename is a directory-entry swap, and the
		// kernel refuses to detach a bind-mounted inode that way: EBUSY,
		// every time. A cross-device mount setup can fail the same rename
		// with EXDEV. The fallback below writes the already-validated
		// bytes into the existing inode instead, which works under a
		// bind mount. It is not atomic: a crash mid-write can leave the
		// file half-written. The backup written just above is what makes
		// that recoverable.
		return writeInPlace(tmpPath, path, mode)
	}
	return nil
}

// writeInPlace rewrites path's existing inode with text's bytes, truncating
// first. Used only when rename cannot swap the target in, such as a
// single-file bind mount; unlike rename, this is not atomic.
func writeInPlace(tmpPath, path string, mode os.FileMode) error {
	text, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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
