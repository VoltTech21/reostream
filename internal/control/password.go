package control

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// The page password, after the claim.
//
// The claim screen sets the password once, on an install that has none,
// and is dead from then on. Until this, that left an operator who wanted a
// different password with nothing but the raw config editor, and an
// operator who had forgotten it with nothing at all. The forgotten case
// still needs the file or the environment, because a password nobody knows
// cannot be changed from behind a login that wants it; this is the other
// case, and it is the common one.

// passwordPage is the change form and whatever came of the last attempt.
type passwordPage struct {
	Title string
	// FromEnv is set when the config reads the password out of an
	// environment variable. The form refuses in that case rather than
	// writing a literal: see serveApplyPassword.
	FromEnv    bool
	EnvVar     string
	Error      string
	ConfigPath string
}

// controlPasswordLine finds the password assignment inside the [control]
// table. It is deliberately not a TOML round trip: config.LoadRaw exists
// because re-encoding a loaded config once wrote every camera's real
// password into the file in place of the $VARIABLE references that were
// there, and this edit must not reintroduce that. The file's own bytes are
// edited, and everything outside the matched line is returned untouched.
var controlPasswordRE = regexp.MustCompile(`(?m)^(\s*password\s*=\s*)(".*"|'.*')\s*$`)

// controlSection returns the byte range of the [control] table in raw, or
// ok false when there is none.
func controlSection(raw string) (start, end int, ok bool) {
	i := strings.Index(raw, "[control]")
	if i < 0 {
		return 0, 0, false
	}
	// The table runs to the next table header at the start of a line.
	rest := raw[i+len("[control]"):]
	next := regexp.MustCompile(`(?m)^\[`).FindStringIndex(rest)
	if next == nil {
		return i, len(raw), true
	}
	return i, i + len("[control]") + next[0], true
}

// currentControlPassword returns the raw value of [control].password as the
// file holds it, quotes stripped, and whether there was one.
func currentControlPassword(raw string) (string, bool) {
	start, end, ok := controlSection(raw)
	if !ok {
		return "", false
	}
	m := controlPasswordRE.FindStringSubmatch(raw[start:end])
	if m == nil {
		return "", false
	}
	return strings.Trim(m[2], `"'`), true
}

// setControlPassword returns raw with [control].password replaced by pw,
// quoted the way the claim quotes it.
func setControlPassword(raw, pw string) (string, error) {
	start, end, ok := controlSection(raw)
	if !ok {
		return "", errors.New("this config has no [control] section to change a password in.")
	}
	section := raw[start:end]
	if !controlPasswordRE.MatchString(section) {
		return "", errors.New("this config's [control] section has no password line to change.")
	}
	replaced := controlPasswordRE.ReplaceAllString(section, "${1}"+fmt.Sprintf("%q", pw))
	return raw[:start] + replaced + raw[end:], nil
}

func (s *Server) servePasswordPage(w http.ResponseWriter, r *http.Request) {
	page := passwordPage{Title: "Password", ConfigPath: s.opts.ConfigPath}
	if raw, err := loadRawConfig(s.opts.ConfigPath); err == nil {
		if cur, ok := currentControlPassword(raw); ok {
			if name, isRef := strings.CutPrefix(cur, "$"); isRef {
				page.FromEnv, page.EnvVar = true, name
			}
		}
	}
	s.render(w, "password.html", page)
}

func (s *Server) serveApplyPassword(w http.ResponseWriter, r *http.Request) {
	page := passwordPage{Title: "Password", ConfigPath: s.opts.ConfigPath}

	raw, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		s.render(w, "password.html", page)
		return
	}

	// An install whose password comes from the environment is refused
	// rather than rewritten. Writing a literal here would work, and would
	// also silently detach the file from the variable the container is
	// started with: the next deploy would set REOSTREAM_CONTROL_PASSWORD
	// to one thing and the page would want another, with nothing on
	// either side saying why. The operator changes it where it lives.
	if cur, ok := currentControlPassword(raw); ok {
		if name, isRef := strings.CutPrefix(cur, "$"); isRef {
			page.FromEnv, page.EnvVar = true, name
			page.Error = fmt.Sprintf("This install reads its password from the environment variable %s, so there is nothing here to change. Set %s to the new password where this daemon is started, then restart it.", name, name)
			s.render(w, "password.html", page)
			return
		}
	}

	current := r.FormValue("current")
	next := r.FormValue("new")

	s.authMu.RLock()
	auth := s.auth
	s.authMu.RUnlock()
	if !auth.AllowNoPassword && !auth.Check(current) {
		page.Error = "That is not the current password."
		s.render(w, "password.html", page)
		return
	}
	if err := validClaimPassword(next); err != nil {
		page.Error = err.Error()
		s.render(w, "password.html", page)
		return
	}
	if next == current {
		page.Error = "That is already the password."
		s.render(w, "password.html", page)
		return
	}

	updated, err := setControlPassword(raw, next)
	if err != nil {
		page.Error = err.Error()
		s.render(w, "password.html", page)
		return
	}
	// The same write path the raw editor and the camera form use, so the
	// backup, the validation and the reload cannot drift apart across a
	// third one.
	if _, err := s.writeAndApply(updated); err != nil {
		page.Error = err.Error()
		s.render(w, "password.html", page)
		return
	}

	s.adoptResolvedPassword(next)
	// Every session, including this one. A password changed while the
	// browsers that knew the old one stay signed in has not really been
	// changed.
	if s.auth.Store != nil {
		s.auth.Store.RevokeAll()
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// serveLogout ends this session. Without it the only way to stop being
// signed in was to clear the cookie by hand.
func (s *Server) serveLogout(w http.ResponseWriter, r *http.Request) {
	s.authMu.RLock()
	auth := s.auth
	s.authMu.RUnlock()
	if ck, err := r.Cookie(auth.CookieName); err == nil && auth.Store != nil {
		auth.Store.Revoke(ck.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
