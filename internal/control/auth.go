package control

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
)

const sessionCookie = "reostream_session"

// sessionStore holds live session tokens in memory. They do not survive a
// restart, which is correct: a restart is also when the password could have
// changed.
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]bool
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: make(map[string]bool)}
}

func (s *sessionStore) issue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = true
	s.mu.Unlock()
	return tok, nil
}

func (s *sessionStore) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[tok]
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
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
