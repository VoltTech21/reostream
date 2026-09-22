package control

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
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
// -- an operator following configPresentMessage's own instructions to
// write a password by hand and THEN restart, a bind mount attaching late, a
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
//     closes the hole. A "$NAME" reference whose variable is not set is
//     the exception: nothing can be adopted, so the page locks and this
//     stays UNclaimed, which is what lets it heal itself if a real
//     password is written later.
//   - [control].allow_no_password: claimed. That is an operator's
//     deliberate choice and the live state already matches it.
//   - no file: unclaimed. The gate stays shut, which is the whole point.
//   - anything else -- unparseable, unreadable, a directory, a file caught
//     half-written, a config with no [control] section at all, a [control]
//     with no answer in it -- NOT claimed.
//     Stricter than the os.Stat it replaces, and it means a half-written
//     config cannot drop the gate. serveClaim separately refuses to write
//     over a file that is already there, so "unclaimed" never costs
//     somebody their cameras.
//
// Adoption only ever tightens. Nothing here turns AllowNoPassword back on,
// and nothing here can leave the page open that was not open already.
//
// The config read deliberately takes no lock of its own: it touches the
// filesystem, and a caller may already hold configMu (serveClaim does).
// Lock order is configMu outer, authMu inner, and only the short
// adopt/settle helpers below take authMu, never across the read.
func (s *Server) claimed() bool {
	s.authMu.RLock()
	// A locked page is not an owned one. The random password behind a lock
	// exists to refuse everybody, not to answer this question, so it must
	// not short-circuit the read below -- that is what would turn a lock
	// into a latch nothing but a restart can clear.
	settled := !s.authLocked && (s.claimSettled || s.auth.Password != "")
	s.authMu.RUnlock()
	if settled {
		return true
	}

	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		return false
	}
	if cfg.Control == nil {
		// NOT claimed, and nothing is settled. A file with no [control]
		// section answers neither question this function asks, so it is
		// the unreadable case wearing a different hat: there is no
		// password to adopt, and counting it as claimed would drop the
		// gate over a process whose live auth state is still the
		// first-run AllowNoPassword default -- serving the config page,
		// and the camera passwords in it, to a request carrying no
		// credential at all. That is not hypothetical: a pre-branch
		// config legitimately has no [control] section, and the
		// refusal's own advice invites writing one by hand.
		//
		// It is tempting to argue the shipped daemon cannot reach here,
		// because it only starts the page when [control] names a
		// listener and config.Validate refuses such a listener with
		// neither a password nor allow_no_password. That argument is
		// about STARTUP, and this branch is reached from the first-run
		// path, which is precisely the one that never ran startup
		// validation: main synthesized a config in memory and is
		// already serving. The file appearing underneath it is the
		// whole scenario.
		//
		// Leaving it unsettled is also what lets the install heal
		// itself: add a password line to that file and the next request
		// adopts it, with no restart.
		return false
	}
	switch {
	case cfg.Control.Password != "":
		return s.adoptPassword(cfg.Control.Password)
	case cfg.Control.AllowNoPassword:
		s.settleClaim()
		return true
	}
	return false
}

// adoptPassword takes a [control].password exactly AS WRITTEN IN THE FILE
// and makes it the one this process requires. It applies the "$NAME" rule
// itself, so it must only ever be handed an unresolved value -- a caller
// holding a secret that config.Load has already resolved wants
// adoptResolvedPassword instead, because a real secret is allowed to begin
// with a "$" and must not be looked up as a variable name.
//
// It reports whether the install counts as claimed.
func (s *Server) adoptPassword(raw string) bool {
	name, isRef := strings.CutPrefix(raw, "$")
	if !isRef {
		s.adoptResolvedPassword(raw)
		return true
	}
	v, set := os.LookupEnv(name)
	if set {
		s.adoptResolvedPassword(v)
		return true
	}

	// The variable the file points at is not set, so this process cannot
	// know the password, and serving on unauthenticated because of it is
	// not an option. Lock instead: refuse everybody until a restart reads
	// the config with the variable present.
	//
	// Deliberately NOT claimed and deliberately not settled. The file
	// exists, so claimGate still sends every route to the claim screen and
	// serveClaim answers 409 without writing anything, which is closed
	// either way -- and leaving it unsettled means an operator who then
	// writes a literal password into the file gets it adopted live, on the
	// next request, instead of the only way out being a restart.
	s.lockPage()
	return false
}

// adoptResolvedPassword makes pw the password this process requires. pw is
// the real secret, already resolved: nothing here interprets a leading "$".
//
// A CHANGED password is adopted too, not just a first one. Rotating the
// password is the thing an operator does after a suspected compromise, and
// a "Saved" banner over a page where the old credential still works and the
// new one does not would be the worst possible answer to it.
func (s *Server) adoptResolvedPassword(pw string) {
	if pw == "" {
		return
	}
	s.authMu.Lock()
	first := s.auth.Password == "" || s.authLocked
	changed := s.auth.Password != pw
	if changed {
		s.auth.Password = pw
		s.auth.AllowNoPassword = false
		s.authLocked = false
		s.claimSettled = true
	}
	s.authMu.Unlock()

	// Never the password itself. Only on a change, so a config read on
	// every request cannot turn into a log line on every request.
	if changed {
		if first {
			log.Printf("reostream: control: a [control] password from %s is now in force; this page requires it",
				s.opts.ConfigPath)
			return
		}
		log.Printf("reostream: control: the [control] password in %s changed; this page now requires the new one and the old one no longer works",
			s.opts.ConfigPath)
	}
}

// lockPage sets a password nothing can match, so every route refuses
// everybody. rand.Text, not the empty string: an empty password would make
// Auth.Check("") succeed, which is the opposite of locking.
func (s *Server) lockPage() {
	s.authMu.Lock()
	locked := !s.authLocked
	if locked {
		s.auth.Password = rand.Text()
		s.auth.AllowNoPassword = false
		s.authLocked = true
	}
	s.authMu.Unlock()

	if locked {
		log.Printf("reostream: control: the config at %s sets [control].password from an environment variable that is not set, so this page is refusing everybody; set the variable and restart, or write a password into the file",
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

// claimPasswordMinLength is the shortest password the claim screen accepts,
// in characters.
//
// This is the ONLY control on the login now, so the arithmetic below is
// the whole security argument and is worth reading before changing the
// number. The private-address gate that used to stand in front of this
// page is gone -- it refused the operator over a tailnet and admitted
// strangers behind any L4 hop. The login throttle that replaced it is gone
// too, because behind docker-proxy a source-keyed limit cannot tell the
// attacker from the operator; serveLogin records the four designs that
// were measured and how each one failed.
//
// The rate to size against is therefore the fastest one measured with no
// limit in the way: ~8,400 password comparisons a second sustained from
// one source, which is what the harness got out of the last two designs
// and is bounded only by the HTTP server. That is 7.3e8 guesses a day and
// 2.65e11 a year. Halving for the expected search, a password must be
// worth about 2^39 to survive a year at that rate.
//
// Against it:
//
//   - The GENERATED password this screen offers first is 16 characters of
//     crypto/rand from a 31-character alphabet: 79.3 bits, 7.7e23. Half of
//     that at 8,400/s is 4.6e11 years. Safe by a margin no rate limit
//     could have bought, which is the point of offering it first.
//   - Three words a person will remember is about 2^33 from an everyday
//     vocabulary: 8.6 billion, half of it 4.3 billion, which at 8,400/s
//     falls in about six DAYS. That is why this screen no longer
//     recommends three words, and why the floor moved off the number that
//     was derived from a throttled rate.
//   - Four such words is about 2^44, which is roughly 33 years. Sixteen
//     characters is about what four words costs to type, which is where
//     the floor comes from.
//
// Be honest about what a length rule does and does not buy: sixteen
// characters a person invents is not 79 bits, and nothing here measures
// entropy. The floor pushes a hand-chosen password past 2^39 in the
// typical case; the generated default is the only thing that guarantees
// it. That is the whole reason the field arrives already filled in.
//
// And this is a default, not an invariant. It applies at claim time only:
// a password written by hand into config.toml, or changed later through
// the Config page, is never checked against it.
//
// Length only: no complexity classes, no strength meter, no dictionary of
// common passwords. This is the first screen a person who does not code
// ever sees, and a rule they satisfy by accepting the password already in
// the box is worth more than one they satisfy by adding "1!" to something
// short.
const claimPasswordMinLength = 16

// validClaimPassword checks a submitted password for the two things that
// would make it unusable in the file it is about to be written into, for
// being empty, and for being long enough to be worth having. It never
// returns the password in its message.
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
	// Characters, not bytes: a password in a language that does not fit in
	// one byte per letter must not be held to a longer rule than an English
	// one. Counted last, so the messages about what the file cannot hold
	// come first -- they are about the password being unusable, not short.
	if utf8.RuneCountInString(pw) < claimPasswordMinLength {
		return fmt.Errorf("a password needs at least %d characters. Any %d will do -- the one already in the box is long enough, and you can use it exactly as it is.",
			claimPasswordMinLength, claimPasswordMinLength)
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
# when reostream falls back to these two defaults. -listen and
# -control-listen on the command line still override them; if you pass
# either, change the matching line below so the two agree.
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

// configPresentMessage is what an operator gets when there is a config file
// but no password can be read out of it. The claim screen shows it INSTEAD
// of the form, and the POST returns it too: learning this after typing a
// password would be learning it too late.
func configPresentMessage(configPath string) string {
	return fmt.Sprintf(
		"There is already a config file at %s, but reostream cannot read a [control] password out of it -- it may not parse, or its [control] section may be incomplete. "+
			"It will not be overwritten, so there is nothing to claim here. "+
			"Fix that file by hand, give its [control] section a password line, and restart reostream.", configPath)
}

type claimPage struct {
	Title string

	// Refused is the explanation shown instead of the form when there is
	// nothing this request can claim: a config file is already there, and
	// it will not be overwritten. Non-empty means no form is drawn:
	// inviting someone to type a password that will be refused on submit
	// is worse than telling them up front.
	Refused string

	// Error is a submission that could not be used: a wrong token, an
	// empty password, a password the config file cannot hold, or a failed
	// write. It never contains the password, and never the token either --
	// the token goes to the daemon's stderr and nowhere else, so a message
	// here may say that one was wrong but must not repeat, echo or hint at
	// the right one.
	//
	// There is deliberately no Token field on this struct, and claim.html
	// has only an empty <input name="token">. The template is never handed
	// the token in any form; anyone checking this page for a leak should be
	// able to see that from the struct alone.
	Error string

	// Suggested is a password generated for this render and printed as
	// readable text in the form. It is the one secret-shaped value that
	// does reach a response body on this page, so it is worth saying
	// exactly why that is not the leak it looks like.
	//
	// It is not a credential until somebody submits it. Every render calls
	// newSuggestedPassword and gets an unrelated 79.3-bit string; nothing
	// stores it, nothing logs it, nothing remembers it between requests,
	// and the server has no idea which of them -- if any -- the operator
	// will send back. An attacker who GETs /claim a thousand times
	// collects a thousand random strings with no relationship to whatever
	// the operator eventually types. What would let them claim the install
	// is the token, which is printed to the daemon's log and to nowhere
	// else, and this route 404s outright once the install is claimed.
	//
	// The containment that keeps that true: claimFormPage is the only
	// thing that sets this field, it is used only on renders that actually
	// draw the form, and claimPage is only ever passed to claim.html. It is
	// never put in an error, never written to the config file, and never
	// passed to log.Printf -- which matters here because the log package
	// tees into the buffer the Logs page serves to anyone signed in.
	//
	// Empty when crypto/rand failed. The form then draws an empty field
	// and the operator types their own, which is the right way for a
	// suggestion to fail.
	Suggested string

	// MinLength is claimPasswordMinLength, passed through so the form's
	// own minlength attribute cannot drift from the rule the server
	// actually enforces. The server check is authoritative either way;
	// this only keeps the browser telling the operator the same number.
	MinLength int
}

// claimFormPage is the claim screen with a freshly generated password in
// it, for every render that draws the form: the first GET, and each
// refusal that invites another try.
//
// Fresh every time, deliberately. Keeping one to re-show after a refusal
// would mean storing a password the operator has not chosen, which is the
// thing this must not do -- and it would give an attacker a value to
// correlate across requests. A rand failure gives an empty suggestion
// rather than an error page: the form still works, it is just not filled
// in.
func claimFormPage(errMsg string) claimPage {
	// The error is dropped on purpose: a rand failure leaves pw empty, the
	// template then draws an empty field, and the operator types their own.
	// Refusing to render the claim screen because a suggestion could not be
	// made would turn a convenience into a way to lock somebody out of
	// their own install.
	pw, err := newSuggestedPassword()
	if err != nil {
		// Named, not silent. The operator sees an empty box and would
		// otherwise find nothing in the log explaining why. The value is
		// not logged because there is no value -- only the failure.
		log.Printf("control: could not generate a password to offer: %v", err)
	}
	return claimPage{
		Title:     "Claim this install",
		Error:     errMsg,
		Suggested: pw,
		MinLength: claimPasswordMinLength,
	}
}

// sameOriginPost reports whether a POST plausibly came from this page.
//
// POST /claim is the one state-changing route with no session behind it,
// by definition: nobody has a password yet. That makes it the one route a
// cross-site request forgery can reach -- an attacker's page can make the
// operator's own browser POST to a private address it cannot itself
// reach. The token blunts this -- an attacker's page does not have the
// token either -- but it does not close it: an operator with the claim
// screen open in one tab has just read the token out of their own logs,
// and a page that can guess or trick them into pasting it would otherwise
// choose the password for an install it cannot even see. The classic home
// router attack, exactly. CSRF is a separate concern from the claim gate,
// and this stays whatever the gate is.
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
	// Unclaimed with a config file already there means a file this daemon
	// could not read a password out of. The POST would refuse it; say so
	// now rather than after a password has been typed. No form is drawn,
	// and so no password is generated: offering one on a page that cannot
	// accept it is noise.
	if _, err := os.Stat(s.opts.ConfigPath); err == nil {
		s.render(w, "claim.html", claimPage{
			Title:   "Claim this install",
			Refused: configPresentMessage(s.opts.ConfigPath),
		})
		return
	}
	s.render(w, "claim.html", claimFormPage(""))
}

// serveClaim checks the one-time token, takes the password, writes the
// config that holds it, and makes it live in this process.
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
	if !sameOriginPost(r) {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "claim.html", claimFormPage("That form was submitted from another site, so reostream did not use it. Open this page directly and set the password here."))
		return
	}

	// The token, before anything else is looked at.
	//
	// Not rate limited, and nothing on this page is: see serveLogin, which
	// records the four source-keyed designs that were built and measured
	// before that was accepted. It would have bought nothing here in any
	// case. The token is 16 characters of crypto/rand from a 31-character
	// alphabet, 79.3 bits: at a million guesses a second an attacker is
	// through the space in rather more than the age of the universe. What
	// it would cost is real -- a delay on this route is something any
	// passer-by could impose on the operator's own claim, and behind
	// docker-proxy the passer-by and the operator are the same key.
	if !claimTokenMatches(s.claimToken(), r.FormValue("token")) {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "claim.html", claimFormPage("That is not the token this reostream printed. It is in the daemon's own log -- `docker logs` on the container, or journalctl on the service -- in a line that begins \"reostream: not yet claimed\". Copy it from there."))
		return
	}

	password := r.FormValue("password")
	if err := validClaimPassword(password); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "claim.html", claimFormPage(err.Error()))
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
		// The token is spent in the same breath: it exists to authorise
		// exactly one claim, and this was it.
		s.authMu.Lock()
		s.auth.Password = password
		s.auth.AllowNoPassword = false
		s.claimTok = ""
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
				Title:   "Claim this install",
				Refused: configPresentMessage(s.opts.ConfigPath),
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "claim.html", claimFormPage(err.Error()))
		return
	}

	// That it was claimed, and from where. Never the password: this line
	// goes to stderr, to `docker logs`, and to the in-memory log buffer
	// the Logs page serves to anyone signed in.
	log.Printf("reostream: control: this install was claimed from %s; the page now requires the password that was set, and the claim token is spent",
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
