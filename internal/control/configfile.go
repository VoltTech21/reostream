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
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// Reloader applies a new camera list to the running fleet. An interface so
// this package does not import the supervisor's construction, only its
// behaviour.
type Reloader interface {
	Reload(cams []config.Camera) (supervisor.ReloadResult, error)

	// Validate reports whether cams would be accepted by Reload, without
	// applying anything. writeAndApply calls this before the config file
	// is written, so a change Reload would refuse is never persisted in
	// the first place; see Supervisor.Validate's own doc comment for the
	// one class of check (RTSP wiring) that only this method, not
	// config.Config.Validate alone, can catch.
	Validate(cams []config.Camera) error
}

// listenersChanged names the listeners whose addresses differ between two
// configs. These cannot be moved on a running process, so the page says so
// instead of saving the value and quietly not applying it, which is the
// failure mode where an operator believes a port changed and it did not.
func listenersChanged(old, next *config.Config) []string {
	var out []string
	if old.Listen != next.Listen {
		out = append(out, "http")
	}
	if rtspListen(old) != rtspListen(next) {
		out = append(out, "rtsp")
	}
	if controlListen(old) != controlListen(next) {
		out = append(out, "control")
	}
	return out
}

func rtspListen(c *config.Config) string {
	if c.RTSP == nil {
		return ""
	}
	return c.RTSP.Listen
}

func controlListen(c *config.Config) string {
	if c.Control == nil {
		return ""
	}
	return c.Control.Listen
}

// renameConfig is os.Rename, kept as a variable so a test can force the
// bind-mount fallback path below without needing a real single-file bind
// mount, which cannot be created in a test.
var renameConfig = os.Rename

// saveConfig validates text and, only if it is a config the daemon would
// boot on AND checkFleet accepts its camera list, replaces the file at
// path. checkFleet may be nil, meaning there is no running fleet to check
// against (no supervisor wired up).
//
// Validation runs the real loader, not a second implementation of it, so
// the browser rejects exactly what startup would reject, including the
// unknown key check that exists to stop a typo becoming a silent 404 at
// three in the morning. checkFleet runs before anything is written for the
// same reason: a config that cannot actually be applied must not land on
// disk just because it is well formed, or a restart is left unable to boot
// from the very file it wrote.
func saveConfig(path, text string, checkFleet func([]config.Camera) error) error {
	// The very first save has nothing to put a temp file next to: the data
	// directory is a fresh volume and may not exist yet. MkdirAll first so
	// that case works; it is a harmless no-op every other time, when the
	// directory is already there. 0700 because the directory exists only
	// to hold config.toml, which is created at 0600 below and full of
	// camera credentials, so no group or other access to either.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Prefer the target's own directory: a temp file there lives on the
	// same filesystem as path, which is what keeps the atomic rename below
	// available. But that directory is /etc/reostream in the shipped image,
	// owned by root, while the daemon runs as the nonroot:nonroot user in
	// the distroless container; nonroot cannot create anything there. When
	// that is why CreateTemp failed, fall back to the system temp dir so
	// validation still has somewhere to put the candidate text, and note
	// that the rename path is now unavailable since the fallback location
	// is not guaranteed to share a filesystem with path.
	tmp, err := os.CreateTemp(dir, ".reostream-config-*")
	sameFS := true
	if err != nil {
		if !errors.Is(err, os.ErrPermission) {
			return err
		}
		sameFS = false
		tmp, err = os.CreateTemp("", ".reostream-config-*")
		if err != nil {
			return err
		}
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

	cfg, err := config.Load(tmpPath)
	if err != nil {
		// config.Load embeds the path it was given in its message, and
		// here that is the temp file's random name, not anything the
		// operator recognizes. Swap in the real path so the message they
		// see matches the file they are editing; the rest of the loader's
		// wording, which names the camera and the key, is left intact.
		return errors.New(strings.Replace(err.Error(), tmpPath, path, 1))
	}

	if checkFleet != nil {
		if err := checkFleet(cfg.Cameras); err != nil {
			return fmt.Errorf("not applying: %w", err)
		}
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

	// The backup normally lives next to the target. But when path's
	// directory refused even a temp file above (the /etc/reostream
	// nonroot case), it will refuse a file named config.toml.bak for the
	// identical reason, so put the backup in the system temp dir instead.
	// It loses nothing by not being beside the target: it is never
	// renamed anywhere, only read back by a human doing recovery.
	backupPath := path + ".bak"
	if !sameFS {
		backupPath = filepath.Join(os.TempDir(), filepath.Base(path)+".bak")
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(backupPath, old, 0o600); err != nil {
			return fmt.Errorf("could not write backup: %w", err)
		}
	}

	// Rename, so a crash mid-write cannot leave a half written config that
	// the next boot refuses. This is the fast, atomic path and is correct
	// wherever the filesystem allows it. When the temp file landed in the
	// system temp dir above, it is not on the same filesystem as path, so
	// a rename would fail with EXDEV every time; skip straight to the
	// in-place write instead of attempting a rename known to fail.
	if sameFS {
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
	return writeInPlace(tmpPath, path, mode)
}

// writeInPlace rewrites path's existing inode with text's bytes, truncating
// first. Used both when rename cannot swap the target in, such as a
// single-file bind mount, and when the temp file could not even be created
// next to path, so no rename was attempted at all. Unlike rename, this is
// not atomic.
func writeInPlace(tmpPath, path string, mode os.FileMode) error {
	text, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// A bare "permission denied" here sends the operator looking
			// at the wrong layer: the daemon already proved it can read
			// and validate this file, so the failure is specifically that
			// the mounted file's owner does not match the container's
			// nonroot user. Say that plainly instead of leaving them to
			// guess. Fixing ownership is the operator's call, not
			// something this daemon should do to a file it does not own.
			return fmt.Errorf("config file %s is not writable by the daemon (usually means its ownership does not match the user the container runs as): %w", path, err)
		}
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
	// Path is the file on disk, shown above the editor. RestartNote says
	// what saving will do before the click, not only after it.
	Path        string
	RestartNote string
}

func (s *Server) serveConfigPage(w http.ResponseWriter, r *http.Request) {
	text, err := loadRawConfig(s.opts.ConfigPath)
	page := configPage{Title: "Config", Text: text, Path: s.opts.ConfigPath, RestartNote: "Saving reloads the cameras whose settings changed."}
	if err != nil {
		page.Error = err.Error()
	}
	s.render(w, "config.html", page)
}

// writeAndApply validates and writes text, applies the new camera list to
// the running fleet, and returns a sentence describing what happened. The
// one caller of this, whether the raw TOML editor or the camera form, is
// what keeps the backup, the validation and the reload from drifting out
// of agreement across two separate write paths.
func (s *Server) writeAndApply(text string) (string, error) {
	// Held across the whole read-validate-write-apply sequence, not just
	// the write: two concurrent saves must not interleave their writes and
	// their applies, or the file on disk and the running fleet can end up
	// describing different configs with neither caller told anything went
	// wrong. See configMu's doc comment on lock ordering.
	s.configMu.Lock()
	defer s.configMu.Unlock()

	before, _ := config.Load(s.opts.ConfigPath)

	var checkFleet func([]config.Camera) error
	if s.opts.Supervisor != nil {
		checkFleet = s.opts.Supervisor.Validate
	}
	if err := saveConfig(s.opts.ConfigPath, text, checkFleet); err != nil {
		return "", err
	}

	after, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		// saveConfig already loaded this file successfully, so reaching here
		// means something changed underneath us.
		return "", err
	}

	// A [control] password that appeared in this save has to become the one
	// this process requires, right now. Exactly one write path used to make
	// auth live -- the claim handler -- and that made this one a trap: an
	// operator on an allow_no_password install who added a password here
	// got a "saved" banner from a page that went on serving without one
	// until somebody restarted the daemon. That includes a password being
	// CHANGED, not just one appearing: rotating it is what an operator does
	// after a suspected compromise, and a save that leaves the old
	// credential working is the worst possible answer to that.
	//
	// adoptResolvedPassword, NOT adoptPassword: after comes from
	// config.Load, which has already resolved any "$NAME" reference, and a
	// real secret is allowed to begin with a "$". Handing a resolved secret
	// to the function that applies the "$NAME" rule would look up the rest
	// of the password as a variable name, find nothing, and lock the page
	// on an unrelated save.
	if after.Control != nil && after.Control.Password != "" {
		s.adoptResolvedPassword(after.Control.Password)
	}

	var note string
	if s.opts.Supervisor != nil {
		res, err := s.opts.Supervisor.Reload(after.Cameras)
		if err != nil {
			return "", fmt.Errorf("saved, but not applied: %w", err)
		}
		note = fmt.Sprintf("%d added, %d removed, %d restarted, %d left alone.",
			res.Added, res.Removed, res.Restarted, res.Unchanged)
	}

	if before != nil {
		if moved := listenersChanged(before, after); len(moved) > 0 {
			note += " Restart required for: " + strings.Join(moved, ", ") + "."
		}
	}
	return note, nil
}

func (s *Server) saveConfigPage(w http.ResponseWriter, r *http.Request) {
	text := r.FormValue("toml")
	note, err := s.writeAndApply(text)
	if err != nil {
		s.render(w, "config.html", configPage{
			Path:  s.opts.ConfigPath,
			Title: "Config", Text: text, Error: err.Error(),
		})
		return
	}
	s.render(w, "config.html", configPage{
		Path: s.opts.ConfigPath, Title: "Config", Text: text, Saved: true, ReloadNote: note,
	})
}
