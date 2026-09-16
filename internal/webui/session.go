// Package webui is the session, auth and rendering plumbing behind the
// reostream daemon's operator page -- one process, one page, covering both
// the fleet and the cameras on it. It holds no policy about that page, only
// the mechanism.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// SessionTTL is an absolute expiry from issue, not a sliding window: these
// pages can read and write camera credentials, so a session that lives
// forever on a shared machine is a real exposure, and there is no renewal on
// activity to defeat it. A long-lived response such as a log stream in
// particular must not count as activity; it is not evidence anyone is still
// at the keyboard.
const SessionTTL = 12 * time.Hour

type SessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
	now    func() time.Time
}

func NewSessionStore() *SessionStore {
	return &SessionStore{tokens: make(map[string]time.Time), now: time.Now}
}

// SetClock overrides the clock so a test can move time instead of sleeping
// out the TTL.
func (s *SessionStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

func (s *SessionStore) Issue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tokens[tok] = s.now().Add(SessionTTL)
	return tok, nil
}

// Valid reports whether tok is a live session. Every call sweeps the whole
// map, not just tok: a token nobody ever presents again, an abandoned cookie
// or a browser that never returns, would otherwise never be checked and
// never removed, and the map would grow without bound.
func (s *SessionStore) Valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	_, ok := s.tokens[tok]
	return ok
}

func (s *SessionStore) sweepLocked() {
	now := s.now()
	for tok, exp := range s.tokens {
		if !now.Before(exp) {
			delete(s.tokens, tok)
		}
	}
}

// len is for tests, which need to see that expiry actually reclaims.
func (s *SessionStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// Auth gates a handler on a session. LoginPath is where an unauthenticated
// request is sent.
//
// CookieName is supplied by the consumer, not a shared package constant.
// There is one surface today, the daemon's operator page, and this package
// holds nothing that assumes it. Two surfaces on one host that shared a
// cookie name would each run their own SessionStore, so logging into one
// would silently log the other out, intermittently, whenever the browser
// resent whichever cookie it last set for that name. That is exactly what
// happened while this project had a second page, and making every caller
// name its own cookie is what stops a second one reintroducing it.
type Auth struct {
	Store           *SessionStore
	Password        string
	AllowNoPassword bool
	LoginPath       string
	CookieName      string
}

// Check compares a submitted password in constant time, so a wrong one
// cannot be found a character at a time by measuring the reply.
func (a Auth) Check(password string) bool {
	return subtle.ConstantTimeCompare([]byte(password), []byte(a.Password)) == 1
}

func (a Auth) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.AllowNoPassword {
			h.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(a.CookieName)
		if err != nil || !a.Store.Valid(ck.Value) {
			http.Redirect(w, r, a.LoginPath, http.StatusSeeOther)
			return
		}
		h.ServeHTTP(w, r)
	})
}
