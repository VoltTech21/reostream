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

	// MinLength is claimPasswordMinLength, passed through for the same
	// reason the claim page passes it: the form's own minlength attribute
	// must not drift from the rule the server enforces. It did -- this
	// template carried a hardcoded 16 and went on advertising it after the
	// floor moved to 12, so the box refused a password serveApplyPassword
	// would have taken.
	MinLength int
}

// controlPasswordLine finds the password assignment inside the [control]
// table. It is deliberately not a TOML round trip: config.LoadRaw exists
// because re-encoding a loaded config once wrote every camera's real
// password into the file in place of the $VARIABLE references that were
// there, and this edit must not reintroduce that. The file's own bytes are
// edited, and everything outside the matched line is returned untouched.
//
// [^\S\n] rather than \s for the horizontal padding, and it matters: \s
// matches a newline, so with (?m) the old `\s*$` ran PAST the end of the
// password line and consumed the blank line after it. The replacement put
// no newline back, so a config whose [control] table is followed by another
// one -- which is every real config, the cameras come after it -- came out
// as `password = "new"[[camera]]` and the daemon then refused to parse its
// own file. See TestChangingThePasswordLeavesAParsableConfig.
var controlPasswordRE = regexp.MustCompile(`(?m)^([^\S\n]*password[^\S\n]*=[^\S\n]*)(".*"|'.*')[^\S\n]*$`)

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
	page := passwordPage{Title: "Password", ConfigPath: s.opts.ConfigPath, MinLength: claimPasswordMinLength}
	if raw, err := loadRawConfig(s.opts.ConfigPath); err == nil {
		if cur, ok := currentControlPassword(raw); ok {
			if name, isRef := strings.CutPrefix(cur, "$"); isRef {
				page.FromEnv, page.EnvVar = true, name
			}
		}
	}
	s.render(w, r, "password.html", page)
}

func (s *Server) serveApplyPassword(w http.ResponseWriter, r *http.Request) {
	page := passwordPage{Title: "Password", ConfigPath: s.opts.ConfigPath, MinLength: claimPasswordMinLength}

	raw, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		s.render(w, r, "password.html", page)
		return
	}

	// An install whose password comes from the environment is a question,
	// not a refusal.
	//
	// It used to be a refusal, on the reasoning that writing a literal
	// would silently detach the file from the variable the container is
	// started with, so the operator should change it where it lives. The
	// reasoning holds; the conclusion did not. The README's only [control]
	// example uses $REOSTREAM_CONTROL_PASSWORD, so every install that
	// followed the documentation landed here, and "change it where it
	// lives" means docker rm and recreate -- which the documentation never
	// said, and which is not a thing to ask of somebody who just wants a
	// password they can remember.
	//
	// So the page explains the consequence and offers to do it anyway.
	// Detaching from the variable is fine when it is chosen; it was only
	// bad when it was silent.
	if cur, ok := currentControlPassword(raw); ok {
		if name, isRef := strings.CutPrefix(cur, "$"); isRef {
			page.FromEnv, page.EnvVar = true, name
			if r.FormValue("detach") == "" {
				page.Error = fmt.Sprintf("This install reads its password from the environment variable %s. Changing it here writes the new password into %s and the daemon stops reading %s -- so remove it from wherever this daemon is started, or the next deploy will look like it changed the password back. Tick the box to go ahead, or set %s where the daemon starts instead.", name, s.opts.ConfigPath, name, name)
				s.render(w, r, "password.html", page)
				return
			}
		}
	}

	current := r.FormValue("current")
	next := r.FormValue("new")

	s.authMu.RLock()
	auth := s.auth
	s.authMu.RUnlock()
	// Through Home Assistant's ingress there is no current password to
	// ask for: the install was claimed with one nobody was shown (see
	// claimGate), and Home Assistant has already signed this user in.
	if !auth.AllowNoPassword && !viaIngress(r) && !auth.Check(current) {
		page.Error = "That is not the current password."
		s.render(w, r, "password.html", page)
		return
	}
	if err := validClaimPassword(next); err != nil {
		page.Error = err.Error()
		s.render(w, r, "password.html", page)
		return
	}
	if next == current {
		page.Error = "That is already the password."
		s.render(w, r, "password.html", page)
		return
	}

	updated, err := setControlPassword(raw, next)
	if err != nil {
		page.Error = err.Error()
		s.render(w, r, "password.html", page)
		return
	}
	// The same write path the raw editor and the camera form use, so the
	// backup, the validation and the reload cannot drift apart across a
	// third one.
	if _, err := s.writeAndApply(updated); err != nil {
		page.Error = err.Error()
		s.render(w, r, "password.html", page)
		return
	}

	s.adoptResolvedPassword(next)
	// Every session, including this one. A password changed while the
	// browsers that knew the old one stay signed in has not really been
	// changed.
	if s.auth.Store != nil {
		s.auth.Store.RevokeAll()
	}
	// Ingress never signs in here, so there is no sign-in page to return
	// to; the fleet is where the operator was.
	if viaIngress(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
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
		Path:     cookiePath(r),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
