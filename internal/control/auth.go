package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const sessionCookie = "reostream_session"

// sessionTTL is an absolute expiry from issue, not a sliding window: this
// page can read and write camera credentials, so a session that lives
// forever on a shared machine is a real exposure, and there is no renewal on
// activity to defeat it. An open /logs/stream connection in particular must
// not count as activity; it is a long-lived response, not evidence anyone is
// still at the keyboard. 12 hours means an operator who uses the page daily
// logs in at most once a day.
const sessionTTL = 12 * time.Hour

// sessionStore holds live session tokens in memory, each with an absolute
// expiry. Tokens do not survive a restart, which is correct: a restart is
// also when the password could have changed.
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token -> expiry

	// now is time.Now, overridable so tests can move the clock instead of
	// sleeping 12 hours.
	now func() time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: make(map[string]time.Time), now: time.Now}
}

// issue mints a session token expiring sessionTTL from now.
func (s *sessionStore) issue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tokens[tok] = s.now().Add(sessionTTL)
	return tok, nil
}

// valid reports whether tok is a live, unexpired session. Every call also
// sweeps the whole map for expired entries, not just tok: that is what
// bounds the map's size without a background goroutine, since a token
// nobody ever presents again (an abandoned cookie, a browser that never
// returns) would otherwise never get checked, let alone removed, on its
// own.
func (s *sessionStore) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	_, ok := s.tokens[tok]
	return ok
}

// sweepLocked deletes every expired entry. Caller holds s.mu.
func (s *sessionStore) sweepLocked() {
	now := s.now()
	for tok, exp := range s.tokens {
		if !now.Before(exp) {
			delete(s.tokens, tok)
		}
	}
}

// authed wraps h so it is only reachable with a session, unless the
// operator turned authentication off on purpose.
func (s *Server) authed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.AllowNoPassword {
			h.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.valid(ck.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) serveLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", struct {
		Title  string
		Failed bool
	}{Title: "Sign in"})
}

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	// Constant time, so a wrong password cannot be found a character at a
	// time by measuring the reply.
	got := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Password)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", struct {
			Title  string
			Failed bool
		}{Title: "Sign in", Failed: true})
		return
	}
	tok, err := s.sessions.issue()
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Matches sessionTTL so the browser drops the cookie at the same
		// moment the server would start rejecting it, rather than holding
		// on to a token that is now dead weight.
		MaxAge: int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
