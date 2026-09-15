package control

import (
	"net"
	"net/http"

	"github.com/VoltTech21/reostream/internal/webui"
)

// loginPage is what login.html renders.
type loginPage struct {
	Title  string
	Failed bool
}

func (s *Server) serveLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", loginPage{Title: "Sign in"})
}

// serveLogin compares the submitted password and answers. There is NOTHING
// in front of it: no rate limit, no lockout, no delay, and that absence is
// a decision rather than an omission.
//
// A rate limit here has to be keyed by something, and the only thing on
// offer is the request's source. Behind docker-proxy -- the shape this
// product ships in, and the one in this repo's own Dockerfile and compose
// file -- every request arrives from 127.0.0.1 or 172.17.0.1, so the
// attacker's key IS the operator's. Four designs were built and measured
// against that, and each failed in one of the only two directions a
// source-keyed limit has:
//
//   - five failures then a five-minute lockout: any passer-by could refuse
//     the OPERATOR's correct password.
//   - a per-request delay with a 64-sleeper cap and a 503 past it: the
//     correct password was refused at 32 requests a second.
//   - a per-key sleeper limit with the overflow checked immediately so
//     nothing is refused: circular, because the attacker creates the
//     overflow. 8,399 comparisons a second at eight workers.
//   - a per-key leaky bucket with a 4,096 global guard: 8,355 comparisons a
//     second at 4,200 connections, and five seconds of ABANDONED requests
//     pushed the operator's own queue 11h27m out.
//
// That is one fact rather than four bugs. Rate limiting works by refusing
// or delaying, and when the attacker is indistinguishable from the victim
// both of those are weapons handed to the attacker. A source-keyed limit
// can refuse the operator, delay the operator, or bound nothing; there is
// no fourth outcome, so there was no fifth round worth running.
//
// The security therefore rests on the one quantity an attacker cannot
// touch: the entropy of the password. The claim screen generates one of
// 79.3 bits and offers it first, and requires at least
// claimPasswordMinLength characters from an operator who types their own.
// See claimPasswordMinLength for the arithmetic against the measured
// worst-case rate of ~8,400 guesses a second.
func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	got := r.FormValue("password")
	// authNow, not the Auth captured at construction: a password set by a
	// claim in this same process has to be the one this login checks
	// against, without a restart.
	auth := s.authNow()
	if !auth.Check(got) {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", loginPage{Title: "Sign in", Failed: true})
		return
	}
	tok, err := s.sessions.Issue()
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Matches webui.SessionTTL so the browser drops the cookie at the
		// same moment the server would start rejecting it, rather than
		// holding on to a token that is now dead weight.
		MaxAge: int(webui.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// hostOfAddr is the address half of a "host:port" remote address. It
// returns the input unchanged when there is no port to strip.
//
// One caller now: the claim log line, which wants to name what was seen.
// A refusal that names nothing is worse than one that names something
// imprecise, which is why this returns the input rather than an error.
// This used to key the login throttle as well, where getting it wrong was
// security-relevant; that throttle is gone (see serveLogin), so what is
// left is a log line.
func hostOfAddr(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}
