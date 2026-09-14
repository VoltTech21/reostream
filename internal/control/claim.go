package control

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/VoltTech21/reostream/internal/config"
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

// claimed reports whether this install has an owner, and brings the
// running auth state into line with the answer on the way past.
//
// It is deliberately not one fact answering two questions. "Should the
// claim screen still be offered?" is a property of the config file;
// "is this process still serving with the wide-open first-run default?"
// is a property of this process. Answering both with a bare os.Stat was a
// hole: a config file appearing by any route other than the claim handler
// -- an operator following claimRefusal's own instructions to write a
// password by hand and THEN restart, a bind mount attaching late, a
// config restored from backup -- dropped the gate, 404'd the claim screen
// and silenced the first-run log, while this process went on serving every
// route, including plaintext camera credentials and camera writes, with
// AllowNoPassword still set. The daemon's own advice opened that window.
//
// So the file is read, not stat'ed, and the auth state is adopted from
// what it says:
//
//   - a live password already held: claimed, nothing to read.
//   - [control].password set: claimed, and adopted, so this process starts
//     requiring it without a restart. Adoption is the half that actually
//     closes the hole.
//   - [control].allow_no_password: claimed. That is an operator's
//     deliberate choice and the live state already matches it.
//   - a config with no [control] section at all: claimed, because the file
//     exists and says nothing about this page, so however this process was
//     configured stands. The shipped daemon never reaches this -- it only
//     starts the page when [control] names a listener, and config.Validate
//     refuses such a listener with neither a password nor
//     allow_no_password -- but a page left open by it is still worth
//     saying out loud, so it logs.
//   - no file: unclaimed. The gate stays shut, which is the whole point.
//   - anything else -- unparseable, unreadable, a directory, a file caught
//     half-written, a [control] with no answer in it -- NOT claimed.
//     Stricter than the os.Stat it replaces, and it means a half-written
//     config cannot drop the gate. serveClaim separately refuses to write
//     over a file that is already there, so "unclaimed" never costs
//     somebody their cameras.
//
// Adoption only ever tightens. Nothing here turns AllowNoPassword on, or
// replaces a password already in hand; a config file is not allowed to
// open up a process that is already closed.
func (s *Server) claimed() bool {
	s.authMu.RLock()
	settled := s.claimSettled || s.auth.Password != ""
	s.authMu.RUnlock()
	if settled {
		return true
	}

	// No lock held across the read: it touches the filesystem, and
	// configMu (which callers such as serveClaim already hold) is the
	// outer lock, authMu the inner one.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		return false
	}
	if cfg.Control == nil {
		if s.settleClaim() && s.authNow().AllowNoPassword {
			log.Printf("reostream: control: the config at %s has no [control] section, so this page is still serving with no password; add a password line under [control] to close it",
				s.opts.ConfigPath)
		}
		return true
	}
	switch {
	case cfg.Control.Password != "":
		s.adoptPassword(cfg.Control.Password)
		return true
	case cfg.Control.AllowNoPassword:
		s.settleClaim()
		return true
	}
	return false
}

// adoptPassword makes a password found in the config file the one this
// process requires, without a restart.
//
// raw is a value as written in the file, so it may be a "$NAME"
// environment reference, which is how the shipped deployment keeps the
// real password out of the file. An unset variable is not a reason to keep
// serving unauthenticated: the install is claimed either way, so the page
// locks instead, with a password nothing can match, until a restart reads
// the config properly. Locked and wrong is recoverable; open is not.
func (s *Server) adoptPassword(raw string) {
	pw := raw
	locked := false
	if name, ok := strings.CutPrefix(raw, "$"); ok {
		if v, set := os.LookupEnv(name); set {
			pw = v
		} else {
			// rand.Text, not the empty string: an empty password would
			// make Auth.Check("") succeed, which is the opposite of
			// locking.
			pw = rand.Text()
			locked = true
		}
	}

	s.authMu.Lock()
	adopted := s.auth.Password == ""
	if adopted {
		s.auth.Password = pw
		s.auth.AllowNoPassword = false
		s.claimSettled = true
	}
	s.authMu.Unlock()

	// Once, because the branch above cannot be taken twice: after it runs
	// the password is no longer empty. Never the password itself.
	if adopted {
		if locked {
			log.Printf("reostream: control: the config at %s sets [control].password from an environment variable that is not set; this page is locked until reostream is restarted",
				s.opts.ConfigPath)
			return
		}
		log.Printf("reostream: control: a config with a [control] password appeared at %s; this page now requires it",
			s.opts.ConfigPath)
	}
}

// settleClaim records that the config file has answered the question, so
// claimed stops re-reading it on every request. Claiming is one way: an
// install that has an owner does not lose one. It reports whether this
// call was the one that settled it, which is what lets a caller log the
// transition exactly once.
func (s *Server) settleClaim() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.claimSettled {
		return false
	}
	s.claimSettled = true
	return true
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
//
// This function sees only the socket peer, which is the source of truth
// for exactly one topology: a browser talking straight to this listener.
// Behind a reverse proxy it is the proxy, and every request on earth then
// arrives from 127.0.0.1 or a Docker bridge address -- both loopback or
// private -- which inverts this whole rule into allow-all. A proxy is a
// more common way to expose an admin page than the raw port forward named
// above, and sameOriginPost's comment below says outright that this page
// can sit behind one. So that case is not decided here, where there is no
// evidence either way; it is decided in proxied, on the request, and a
// request showing any sign of having been forwarded is refused. See
// proxied.
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

// proxied reports whether a request reached this daemon through something
// that forwards, which makes its socket peer meaningless as evidence of
// who sent it.
//
// The headers are not parsed and not trusted, only noticed. Their presence
// alone is proof that mayClaim is looking at a hop rather than a claimer:
// behind Caddy, nginx, Traefik, or a Docker port publish that goes through
// the userland proxy, the peer is 127.0.0.1 or 172.17.0.1, so a stranger
// on the internet passes the private-address rule and the whole rule
// inverts into allow-all. Trusting the header's contents instead would be
// worse -- anyone can send one -- so the only safe reading is that a
// forwarded request cannot be shown to be local, and a claim that cannot
// be shown to be local is refused.
//
// A forged header on a direct connection therefore costs an attacker a
// refusal, never an approval, which is the right way round. The refusal
// names two ways forward, and neither needs the proxy to cooperate.
func proxied(r *http.Request) bool {
	return r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != ""
}

// claimRefusal is what a refused claimer reads, or "" when this request
// may claim. A bare 403 is a dead end for a non-coder on their first run,
// so each refusal says what was seen, why it was refused, and what to do
// instead.
func claimRefusal(r *http.Request, configPath string) string {
	where := configPath
	if where == "" {
		where = "reostream's config file"
	}
	byHand := fmt.Sprintf("write a [control] section with a password line into %s, then restart reostream.", where)

	if proxied(r) {
		return "This request arrived through a proxy -- it carries a forwarding header -- so reostream cannot tell where it really came from, " +
			"and an install can only be claimed from the local network. " +
			"Two ways forward. Reach this page directly rather than through the proxy, from a machine on the same network as reostream. " +
			"Or set the password by hand: " + byHand
	}
	if !mayClaim(r.RemoteAddr) {
		return fmt.Sprintf(
			"This install can only be claimed from the local network, and this request came from %s, which is not a local address. "+
				"Addresses in 100.64.0.0/10 count as not local too: that range is where Tailscale lives, but it is also shared ISP carrier-grade NAT, and an address alone cannot tell the two apart. "+
				"Two ways forward. Open this page again from a machine on the same network as reostream, which will reach it from a 192.168.x.x, 10.x.x.x or 172.16-31.x.x address. "+
				"Or set the password by hand: "+byHand,
			hostOfAddr(r.RemoteAddr))
	}
	return ""
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
	// Before ranging: a byte that is not valid UTF-8 comes out of a range
	// loop as U+FFFD, which passes every check below, and then %q writes
	// it as an escape the TOML reader reads back as a different character
	// entirely. The password would work until the next restart and not
	// after it. No browser can send this; curl --data-binary can.
	if !utf8.ValidString(pw) {
		return errors.New("that password is not valid text. Type it again, using characters your keyboard produces.")
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

// errAlreadyClaimed is the re-check inside the claim's lock finding that
// another claim got there first.
var errAlreadyClaimed = errors.New("this install has already been claimed")

// errConfigPresent is a claim on an install that is unclaimed but does have
// a config file: a broken one. Writing would destroy it.
var errConfigPresent = errors.New("a config file is already there")

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
	page.Refused = claimRefusal(r, s.opts.ConfigPath)
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
	if why := claimRefusal(r, s.opts.ConfigPath); why != "" {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "claim.html", claimPage{
			Title:   "Claim this install",
			Refused: why,
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
			return errAlreadyClaimed
		}
		// Unclaimed does not always mean there is no file: a config that
		// will not parse, or one whose [control] section answers neither
		// question, leaves the gate shut on purpose. A claim writes a
		// whole file, so writing one here would destroy whatever cameras
		// that file holds. Refuse instead, and say what to do.
		if _, statErr := os.Stat(s.opts.ConfigPath); statErr == nil {
			return errConfigPresent
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
		// A claim that lost a race with another claim gets the answer a
		// late claim gets anywhere else on this route: the screen is gone,
		// because the install now has an owner. Only a real write failure
		// is a 500.
		if errors.Is(err, errAlreadyClaimed) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, errConfigPresent) {
			w.WriteHeader(http.StatusConflict)
			s.render(w, "claim.html", claimPage{
				Title: "Claim this install",
				Error: fmt.Sprintf("There is already a config file at %s, but reostream cannot read a [control] password out of it -- it may not parse, or the section may be incomplete. "+
					"It will not be overwritten. Fix that file by hand, give its [control] section a password line, and restart reostream.", s.opts.ConfigPath),
			})
			return
		}
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
