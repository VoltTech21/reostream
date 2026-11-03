// Package webui is the session, auth and rendering plumbing behind the
// reostream daemon's operator page -- one process, one page, covering both
// the fleet and the cameras on it. It holds no policy about that page, only
// the mechanism.
package webui

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
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

// Revoke ends one session, for a sign out. A token that is not there is
// not an error: a cookie can outlive the session it names.
func (s *SessionStore) Revoke(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, tok)
}

// RevokeAll ends every session.
//
// This is what a password change has to call. Changing the password
// without it leaves every browser that was already signed in signed in,
// including the one whose access was the reason for changing it, which
// makes the change worth very little.
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.tokens)
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

// loginKDFIterations is how much work one password check costs.
//
// It is the only thing bounding how fast this page can be guessed at, and
// that is deliberate: see serveLogin in internal/control for the four
// source-keyed rate limits that were built, measured and removed. Every one
// of them failed the same way -- behind docker-proxy the attacker's key IS
// the operator's, so a limit can refuse the operator, delay the operator,
// or bound nothing.
//
// Cost per guess is the one lever an attacker cannot turn, because the work
// happens on this server rather than on theirs. A single SHA-256 was free,
// which is why the measured worst case was ~8,400 guesses a second. At
// 100,000 iterations one derivation takes ~83ms, and a check does two, so a
// guess costs ~166ms of a core: roughly 50 a second with every core on this
// machine saturated, against 8,400 before. That is the ~2^7.5 that lets
// claimPasswordMinLength come down from 16 characters to 12.
//
// Nothing here can lock anybody out. There is no key, no counter and no
// state: the operator pays the same ~166ms once, which is imperceptible,
// and an attacker flooding the endpoint burns CPU without ever being able
// to refuse the operator a turn.
const loginKDFIterations = 100_000

// loginKDFSalt is fixed rather than random, and that is correct HERE
// although it would be wrong almost anywhere else.
//
// A salt defends a STORED hash: it stops one table of precomputed hashes
// breaking every account at once. Nothing is stored here. The password
// lives in the config file as text, this derivation exists only to make an
// ONLINE guess expensive, and the result is thrown away after the compare.
// There is one account and no database to steal, so there is nothing for a
// per-install salt to protect. A fixed one keeps Auth a plain value that
// can be copied and compared without carrying state.
var loginKDFSalt = []byte("reostream login kdf v1")

// stretch is PBKDF2-HMAC-SHA256 of password over loginKDFSalt, one 32
// byte key. It was written out by hand before crypto/pbkdf2 joined the
// standard library (Go 1.24); TestStretchMatchesStandardPBKDF2 pins that the
// two give the same bytes.
func stretch(password string, iter int) ([32]byte, error) {
	var out [32]byte
	k, err := pbkdf2.Key(sha256.New, password, loginKDFSalt, iter, len(out))
	if err != nil {
		return out, err
	}
	copy(out[:], k)
	return out, nil
}

// Check compares a submitted password in constant time, so a wrong one
// cannot be found a character at a time by measuring the reply.
//
// Derived first, rather than compared directly, for two separate reasons.
//
// Length: subtle.ConstantTimeCompare returns 0 immediately when the two
// lengths differ, so comparing the raw strings is constant time in their
// CONTENT and not in their LENGTH. A guesser could learn how long the real
// password is by timing replies. Two derived keys are always the same size,
// so the comparison below cannot short circuit on anything.
//
// Cost: see loginKDFIterations. Both sides are derived on every call, not
// just the submitted one. Deriving the configured password once and caching
// it would save the operator ~83ms and halve the attacker's cost per guess,
// which is the wrong way round.
func (a Auth) Check(password string) bool {
	// A derivation that fails refuses the login: two failed derivations
	// would otherwise compare equal and let any password in.
	got, err := stretch(password, loginKDFIterations)
	if err != nil {
		return false
	}
	want, err := stretch(a.Password, loginKDFIterations)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
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
