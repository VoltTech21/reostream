package control

import (
	"net/http"

	"github.com/VoltTech21/reostream/internal/webui"
)

// loginPage is what login.html renders. Failed and TooMany are separate
// because they are different instructions: one says type it again, the
// other says stop typing for a while.
type loginPage struct {
	Title   string
	Failed  bool
	TooMany string
}

func (s *Server) serveLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", loginPage{Title: "Sign in"})
}

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	// The same throttle the claim token uses, keyed the same way. This page
	// has no password-strength rule on purpose -- an operator picks what
	// they pick -- so an unthrottled login is a guess loop against whatever
	// they picked, run by anyone who can reach the port.
	who := hostOfAddr(r.RemoteAddr)
	if left, blocked := s.throttle.blocked(who); blocked {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login.html", loginPage{Title: "Sign in", TooMany: tooManyMessage(left)})
		return
	}

	got := r.FormValue("password")
	// authNow, not the Auth captured at construction: a password set by a
	// claim in this same process has to be the one this login checks
	// against, without a restart.
	auth := s.authNow()
	if !auth.Check(got) {
		s.throttle.fail(who)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", loginPage{Title: "Sign in", Failed: true})
		return
	}
	// Failures only, and a success forgets them: an operator who mistypes
	// twice and then gets it right must not carry those two failures into
	// their next sign-in.
	s.throttle.clear(who)
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
