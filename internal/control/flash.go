// Flash: the one line a write leaves behind for the page it sends the
// operator back to.
//
// Every write on this page used to land on result.html, a full page of
// outcome, detail, the whole Before document and the whole After document,
// whose only way onward was a "restore previous" button. There was no way
// back to the thing being configured, and a refresh re-POSTed the write.
// A curated setting, the floodlight and the NTP form now write, stash one
// of these, and redirect back to the page the setting belongs to, so a
// refresh is a plain GET and the operator is left looking at the control
// they just changed. The raw block editor (serveWrite) deliberately keeps
// result.html: seeing the exact before/after XML is the entire point of
// that page.
//
// The flash is held here, server side, and never in the redirect URL. The
// undo value is the camera's previous document; a query string would write
// it into browser history and into every access log between here and the
// browser, and this project's rule is that values do not reach URLs.
package control

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// flashTTL is how long a stashed banner is worth showing. It is short on
// purpose: this exists to cover one redirect, and an operator who never
// comes back should not have their camera's previous document sitting in
// this process an hour later.
const flashTTL = 5 * time.Minute

// flashCookie keys a flash for an install running with no password, where
// there is no session cookie to key by. It carries a random id and nothing
// else -- never the message, never the undo document.
const flashCookie = "reostream_flash"

// Flash is what a banner renders: which of the three write outcomes this
// was, one line of text, and optionally the undo that puts the previous
// value back.
//
// Outcome is the same vocabulary WriteResult uses -- confirmed, accepted,
// refused -- because the banner must not soften a refusal into a tick. The
// template picks its colour from it, out of the same three variables
// style.css already spends on write outcomes.
type Flash struct {
	Outcome string
	Message string

	// UndoAction, when non-empty, renders a small form posting
	// UndoValue as UndoParam (plus UndoHidden) to UndoAction: the same
	// verified write path result.html's "restore previous" button uses,
	// just presented as one word beside the message rather than as a
	// section of its own.
	//
	// There is no confirm dialog on it. It is one click, and it is
	// reversible the same way the write that produced it was. The confirm
	// gates that do exist -- Group.ConfirmReason on Lights and IR, and
	// the floodlight's own -- are about a physical effect and stay where
	// they are.
	UndoAction string
	UndoParam  string
	UndoValue  string
	UndoHidden map[string]string
}

// FleetFlash is what an every-camera apply leaves behind: one row per
// camera, and which setting those rows are about.
//
// It is stashed here rather than rendered straight onto the POST response
// for exactly the reason a single write is: a rendered POST means refresh
// re-submits, and re-submitting this one writes the whole fleet a second
// time. The blast radius is what makes it worth carrying rows through a
// redirect at all -- a banner is one line and rows are as many lines as
// there are cameras, but the rule about refresh does not care about size.
//
// Results is never collapsed into a summary on the way here; see applyAll
// in fleet.go for why a fleet of three models cannot honestly be reduced
// to one line.
type FleetFlash struct {
	// Target names which setting the rows belong to ("ntp", "timezone").
	// The two settings go out as two different CGI commands, and a camera
	// refusing one must not be read as it refusing the other.
	Target  string
	Results []FleetResult
}

// flashEntry is what one browser has waiting for it: the one-line banner a
// single write left, the rows an every-camera apply left, or both, and the
// moment they stop being worth showing.
//
// Both are pointers and both are optional, so that taking one does not
// discard the other. A single-camera write and a fleet apply report through
// different fields of the same entry rather than through two stores,
// because they share everything that matters here: the key, the TTL, the
// sweep, and the read-once rule.
type flashEntry struct {
	flash   *Flash
	fleet   *FleetFlash
	expires time.Time
}

// flashFor turns a finished write into the line a banner shows. subject
// names what was changed ("lounge: camera name"); the outcome supplies the
// rest.
//
// Only a failure carries Detail. A confirmed write does not need a
// paragraph explaining itself, which is the whole complaint this change
// answers; an accepted or refused one does, because "accepted" means the
// camera answered 200 and nothing here can prove effect, and a refusal is
// useless without the reason.
func flashFor(subject string, result WriteResult) Flash {
	f := Flash{Outcome: result.Outcome}
	switch result.Outcome {
	case "confirmed":
		f.Message = subject + " saved"
	case "accepted":
		f.Message = subject + " sent, but not confirmed: " + result.Detail
	default:
		f.Message = subject + " refused: " + result.Detail
	}
	return f
}

// flashKey names whose flash this is.
//
// The session cookie is the right key and the one used whenever there is
// one: it is already on the request, it is per browser, and it never
// reaches a URL. An install running with AllowNoPassword has no session at
// all, so setFlash mints a dedicated random id cookie for it instead,
// rather than falling back to something shared like the remote address --
// behind docker-proxy every request arrives from one address, which would
// hand one operator's banner to another.
//
// An empty key means "no flash for this request", which is what a GET with
// neither cookie gets: nothing to show, nothing stored.
func (s *Server) flashKey(r *http.Request) string {
	if ck, err := r.Cookie(s.authNow().CookieName); err == nil && ck.Value != "" {
		return "session:" + ck.Value
	}
	if ck, err := r.Cookie(flashCookie); err == nil && ck.Value != "" {
		return "flash:" + ck.Value
	}
	return ""
}

// newFlashID is the value of the fallback cookie: a random id and nothing
// else. It is not a credential -- it authorises nothing, and an install
// with no password has no session for it to stand in for -- but it is
// generated the same way anyway, because guessing it would mean picking up
// somebody else's banner and the undo document in it.
func newFlashID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// flashKeyFor is flashKey for a request that is about to stash something:
// when there is no key to be had it mints the fallback cookie and returns
// that instead, so the redirect the caller is about to write carries the
// id its landing GET will be found by.
//
// It returns "" only when even that failed, which the callers treat as
// "report nothing": neither a banner nor a table of rows is worth failing
// a write that has already reached the cameras.
func (s *Server) flashKeyFor(w http.ResponseWriter, r *http.Request) string {
	if key := s.flashKey(r); key != "" {
		return key
	}
	tok, err := newFlashID()
	if err != nil {
		return ""
	}
	ck := &http.Cookie{
		Name:     flashCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(flashTTL.Seconds()),
	}
	http.SetCookie(w, ck)
	// Put it on the request too, so a second stash in the same request
	// finds the id this one just minted instead of minting another. Two
	// ids would mean two entries under two keys, and the browser is only
	// going to send one of them back.
	r.AddCookie(ck)
	return "flash:" + tok
}

// stash puts fill's changes into this request's entry, creating it if there
// is none and refreshing its expiry either way. The entry is read first so
// that stashing a banner does not throw away rows already waiting, or the
// other way round.
func (s *Server) stash(w http.ResponseWriter, r *http.Request, fill func(*flashEntry)) {
	key := s.flashKeyFor(w, r)
	if key == "" {
		return
	}
	s.flashMu.Lock()
	defer s.flashMu.Unlock()
	s.sweepFlashesLocked()
	e := s.flashes[key]
	fill(&e)
	e.expires = s.flashClock().Add(flashTTL)
	s.flashes[key] = e
}

// setFlash stashes f for whoever made this request, minting the fallback
// cookie when there is no session to key by. Call it before writing the
// redirect: it may need to set a header.
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, f Flash) {
	s.stash(w, r, func(e *flashEntry) { e.flash = &f })
}

// setFleetFlash stashes an every-camera apply's rows for the page the
// operator is about to be redirected to. Same call order as setFlash: it
// may need to set a header, so it goes before the redirect is written.
func (s *Server) setFleetFlash(w http.ResponseWriter, r *http.Request, f FleetFlash) {
	s.stash(w, r, func(e *flashEntry) { e.fleet = &f })
}

// takeFlash returns this request's stashed banner and removes it, so it is
// shown exactly once: a banner that survived a reload would go on claiming
// a write just happened long after it did.
//
// It returns a pointer because the page structs carry one, and nil is how
// a template says "no banner" without a second boolean field.
func (s *Server) takeFlash(r *http.Request) *Flash {
	var f *Flash
	s.unstash(r, func(e *flashEntry) { f, e.flash = e.flash, nil })
	return f
}

// takeFleetFlash returns this request's stashed every-camera rows and
// removes them, on the same read-once rule takeFlash follows: rows that
// survived a reload would go on claiming the fleet was just written long
// after it was.
func (s *Server) takeFleetFlash(r *http.Request) *FleetFlash {
	var f *FleetFlash
	s.unstash(r, func(e *flashEntry) { f, e.fleet = e.fleet, nil })
	return f
}

// unstash hands this request's entry to take, which clears the one field it
// is after, and then keeps the entry only if something is still waiting in
// it. Taking a banner must not discard rows stashed beside it, and taking
// rows must not discard the banner.
func (s *Server) unstash(r *http.Request, take func(*flashEntry)) {
	key := s.flashKey(r)
	if key == "" {
		return
	}
	s.flashMu.Lock()
	defer s.flashMu.Unlock()
	s.sweepFlashesLocked()
	e, ok := s.flashes[key]
	if !ok {
		return
	}
	take(&e)
	if e.flash == nil && e.fleet == nil {
		delete(s.flashes, key)
		return
	}
	s.flashes[key] = e
}

// sweepFlashesLocked drops every expired entry, not just the one being
// looked at, on the same reasoning webui.SessionStore sweeps on every
// call: an operator who submits a write and then closes the tab never
// comes back for their entry, and without this the map would only ever
// grow. Each entry holds a camera's whole previous document, so that is
// memory worth reclaiming rather than a few bytes.
//
// Callers hold flashMu.
func (s *Server) sweepFlashesLocked() {
	now := s.flashClock()
	for key, e := range s.flashes {
		if !now.Before(e.expires) {
			delete(s.flashes, key)
		}
	}
}

// flashClock is time.Now unless a test has moved it, so expiry can be
// tested by moving time rather than by sleeping out the TTL.
func (s *Server) flashClock() time.Time {
	if s.flashNow != nil {
		return s.flashNow()
	}
	return time.Now()
}

// flashLen is for tests, which need to see that expiry actually reclaims.
func (s *Server) flashLen() int {
	s.flashMu.Lock()
	defer s.flashMu.Unlock()
	return len(s.flashes)
}

// returnTo picks where a write sends the operator back to: the page they
// came from when the form named one, and fallback otherwise.
//
// A form may name only a path on this same page. Anything else -- an
// absolute URL, a protocol-relative "//host", a path with a header break
// in it -- is ignored rather than refused, because a write that already
// reached the camera must not be reported as a bad request; the operator
// simply lands on the page the setting belongs to.
//
// The status page is why this exists at all: its quick overlay switches
// post through the curated-setting handler, and an operator toggling a
// switch on the fleet view expects to still be on the fleet view
// afterwards, not on one camera's page.
func returnTo(r *http.Request, fallback string) string {
	v := r.FormValue("return")
	if v == "" || !strings.HasPrefix(v, "/") {
		return fallback
	}
	// "//host" and "/\host" are both read as another origin by browsers.
	if strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") {
		return fallback
	}
	if strings.ContainsAny(v, "\r\n") {
		return fallback
	}
	return v
}

// lowerFirst lower-cases the first letter of a curated field's Label so it
// reads as part of a sentence in a banner. The labels are written for a
// table cell ("Camera name"), and this page's copy is lower case.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
