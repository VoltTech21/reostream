package control

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// The listen addresses a claim writes into the config it creates. They
// duplicate defaultListen and defaultControlListen in cmd/reostream/main.go,
// which is a command and so cannot be imported from here.
//
// Writing them is safe rather than a guess: an install is only claimable
// while it has no config file at all (see claimed), and no config file is
// exactly the state in which main synthesizes those two defaults. So the
// values written here are the ones the daemon is already listening on. The
// one exception is an explicit -listen flag, which overrides the file; the
// header comment in the written config says so.
const (
	claimDefaultListen        = "0.0.0.0:8560"
	claimDefaultControlListen = "0.0.0.0:8562"
)

// claimed reports whether this install has an owner.
//
// A password held by the running server is the primary answer, and it is
// read from the live auth state, not from Options, so a claim takes effect
// in this process the moment it is made rather than at the next restart.
//
// With no password the install is unclaimed only if there is also no config
// file. An operator who wrote a config by hand -- including one that sets
// allow_no_password = true, which is a deliberate choice this daemon has
// always supported -- has already configured this install, and must not be
// shown a claim screen over the top of it, nor have that file replaced by
// whoever POSTs /claim first.
//
// Fail closed on a stat error that is not "no such file": an unreadable
// data directory makes the install unclaimable, never claimable.
func (s *Server) claimed() bool {
	s.authMu.RLock()
	pw := s.auth.Password
	s.authMu.RUnlock()
	if pw != "" {
		return true
	}
	_, err := os.Stat(s.opts.ConfigPath)
	return !errors.Is(err, fs.ErrNotExist)
}

// mayClaim reports whether a request from remoteAddr is allowed to claim
// this install.
//
// The hazard this exists for: claiming on first visit means the first
// person to arrive owns the install, and this install can write to cameras
// -- credentials, accounts, time, every curated setting. If the control
// port is exposed to the internet, whether by a port forward, a DMZ host,
// or a container published on 0.0.0.0, then "the first person to arrive"
// is a stranger with a scanner, and the install is theirs before the
// operator ever opens the page. Requiring a private source address keeps
// the claim to whoever is already inside the network.
//
// Loopback, link-local and the RFC1918 private ranges only. A source that
// cannot be parsed is not private: fail closed.
//
// Deliberately NOT included: 100.64.0.0/10. netip's IsPrivate returns
// false for it, and that is the behaviour kept here on purpose. It is
// where every Tailscale address lives, so an operator reaching a fresh
// install over a tailnet is refused on their own daemon -- an unhappy but
// accepted cost, because the same range is also real ISP carrier-grade
// NAT, shared with every other subscriber behind that CGNAT box. An
// address out of 100.64/10 cannot tell a tailnet peer from an ISP
// neighbour, so widening the rule to admit the first would admit the
// second too. Do not "fix" this by adding the range; the refusal names the
// two ways out instead (see claimRefusal).
func mayClaim(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsPrivate()
}

// hostOfAddr is the address half of a "host:port" remote address, for
// showing an operator what was seen. It returns the input unchanged when
// there is no port to strip, so a refusal always names something.
func hostOfAddr(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

// claimRefusal is what a refused claimer reads. A bare 403 is a dead end
// for a non-coder on their first run, so this names the address that was
// seen, says plainly why it was refused, and gives both ways forward.
func claimRefusal(remoteAddr, configPath string) string {
	where := configPath
	if where == "" {
		where = "reostream's config file"
	}
	return fmt.Sprintf(
		"This install can only be claimed from the local network, and this request came from %s, which is not a local address. "+
			"Addresses in 100.64.0.0/10 count as not local too: that range is where Tailscale lives, but it is also shared ISP carrier-grade NAT, and an address alone cannot tell the two apart. "+
			"Two ways forward. Open this page again from a machine on the same network as reostream, which will reach it from a 192.168.x.x, 10.x.x.x or 172.16-31.x.x address. "+
			"Or set the password by hand: write a [control] section with a password line into %s, then restart reostream.",
		hostOfAddr(remoteAddr), where)
}

// validClaimPassword checks a submitted password for the two things that
// would make it unusable in the file it is about to be written into, and
// for being empty. It never returns the password in its message.
func validClaimPassword(pw string) error {
	if pw == "" {
		return errors.New("choose a password.")
	}
	if strings.HasPrefix(pw, "$") {
		return errors.New("a password cannot start with \"$\". In this config file a leading $ means \"read this from the environment variable of that name\", so reostream would go looking for a variable instead of using what you typed.")
	}
	for _, r := range pw {
		if r < 0x20 || r == 0x7f {
			return errors.New("a password cannot contain control characters or line breaks.")
		}
	}
	return nil
}

// claimConfigText is the whole config file a claim writes: the listen
// addresses the daemon is already running on, and the [control] section
// that makes the password real. %q is TOML-safe for a password that has
// passed validClaimPassword -- the escapes Go emits for a quote and a
// backslash are the ones TOML's basic strings take, and the values that
// would differ, control characters, are refused before reaching here.
func claimConfigText(password string) string {
	return fmt.Sprintf(`# reostream config.
#
# Written when this install was claimed. Everything here can be edited by
# hand, or from the Config page.
#
# The listen addresses are the ones this daemon was already running on: an
# install is only claimable while it has no config at all, which is exactly
# when reostream falls back to these two defaults. A -listen flag on the
# command line still overrides the first one; if you pass one, change the
# line below to match it.
#
# Add cameras from the Setup page, or by hand as [[camera]] blocks.

listen = %q

[control]
listen = %q
password = %q
`, claimDefaultListen, claimDefaultControlListen, password)
}

type claimPage struct {
	Title string

	// Refused is the explanation shown instead of the form when the
	// source address may not claim. Non-empty means no form is drawn:
	// inviting someone to type a password that will be refused on submit
	// is worse than telling them up front.
	Refused string

	// Error is a submission that could not be used: an empty password, a
	// password the config file cannot hold, or a failed write. It never
	// contains the password.
	Error string
}

// sameOriginPost reports whether a POST plausibly came from this page.
//
// POST /claim is the one state-changing route with no session behind it,
// by definition: nobody has a password yet. That makes it the one route a
// cross-site request forgery can reach -- an attacker's page can make the
// operator's own browser POST to a private address, and mayClaim sees the
// operator's LAN address and is satisfied, so the attacker would choose
// the password for an install they cannot even see. The classic home
// router attack, exactly.
//
// Every browser in use sends Origin on a cross-site form POST, so
// requiring it to match this host closes that. A request with no Origin at
// all is allowed: that is a non-browser client such as curl, which carries
// no ambient authority for an attacker's page to borrow in the first
// place.
//
// The session-backed POST routes on this page do not need this check: the
// session cookie is SameSite=Lax (see serveLogin), which browsers do not
// attach to a cross-site POST, so a forged request arrives with no session
// and is turned away by the auth wrapper before any handler sees it.
func sameOriginPost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Host only, not scheme: this page is commonly reached over plain
	// HTTP on a LAN and can sit behind a TLS-terminating proxy, so the
	// scheme the browser saw is not reliably the one the request arrives
	// with. The host is what identifies the origin here.
	return u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// serveClaimForm draws the claim screen. On a claimed install it is a 404:
// the route does not exist any more, and saying so is better than hinting
// that it once did.
func (s *Server) serveClaimForm(w http.ResponseWriter, r *http.Request) {
	if s.claimed() {
		http.NotFound(w, r)
		return
	}
	page := claimPage{Title: "Claim this install"}
	if !mayClaim(r.RemoteAddr) {
		page.Refused = claimRefusal(r.RemoteAddr, s.opts.ConfigPath)
	}
	s.render(w, "claim.html", page)
}

// serveClaim takes the password, writes the config that holds it, and
// makes it live in this process.
//
// Order matters and is not incidental: the file is written first and the
// running server's auth state is changed only after that write succeeds. A
// claim that cannot write its config must leave the process still
// unclaimed, so the operator gets the claim screen again rather than an
// install that believes it has an owner and has nothing on disk to prove
// it after a restart.
func (s *Server) serveClaim(w http.ResponseWriter, r *http.Request) {
	if s.claimed() {
		http.NotFound(w, r)
		return
	}
	if !mayClaim(r.RemoteAddr) {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "claim.html", claimPage{
			Title:   "Claim this install",
			Refused: claimRefusal(r.RemoteAddr, s.opts.ConfigPath),
		})
		return
	}
	if !sameOriginPost(r) {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "claim.html", claimPage{
			Title: "Claim this install",
			Error: "That form was submitted from another site, so reostream did not use it. Open this page directly and set the password here.",
		})
		return
	}

	password := r.FormValue("password")
	if err := validClaimPassword(password); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "claim.html", claimPage{Title: "Claim this install", Error: err.Error()})
		return
	}

	// configMu, the same lock every other write to this file takes, so two
	// claim attempts cannot both find the install unclaimed and both
	// write. saveConfig, not a private write path: it validates through
	// the real loader, keeps a backup, renames atomically with the
	// in-place fallback a bind-mounted config needs, and creates the data
	// directory a fresh volume does not have yet.
	s.configMu.Lock()
	err := func() error {
		if s.claimed() {
			return errors.New("this install has already been claimed.")
		}
		// checkFleet is nil: a claim writes no cameras, so there is no
		// camera list for the supervisor to have an opinion about.
		return saveConfig(s.opts.ConfigPath, claimConfigText(password), nil)
	}()
	if err == nil {
		// Live, under the write lock every request's auth read takes, so
		// the next request through the wrapper is challenged. No restart.
		s.authMu.Lock()
		s.auth.Password = password
		s.auth.AllowNoPassword = false
		s.authMu.Unlock()
	}
	s.configMu.Unlock()

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "claim.html", claimPage{Title: "Claim this install", Error: err.Error()})
		return
	}

	// That it was claimed, and from where. Never the password: this line
	// goes to stderr, to `docker logs`, and to the in-memory log buffer
	// the Logs page serves to anyone signed in.
	log.Printf("reostream: control: this install was claimed from %s; the page now requires the password that was set",
		hostOfAddr(r.RemoteAddr))

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// claimGate sends every request on an unclaimed install to the claim
// screen, except the claim screen itself and the stylesheet it needs to
// render. It wraps the whole route table rather than sitting in one
// handler, so a route added later cannot forget it.
//
// This runs ahead of the auth wrapper and ahead of serveDashboard's own
// "no cameras yet, go to setup" redirect, which fixes their precedence:
// unclaimed beats no-cameras. An unclaimed install has no owner, so
// nobody should be adding cameras to it -- or reading the credentials of
// cameras already on it -- until somebody has taken it.
func (s *Server) claimGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.claimed() || r.URL.Path == "/claim" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/claim", http.StatusSeeOther)
	})
}
