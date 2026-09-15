package control

import (
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

func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	// The password, unlike the claim token, is operator-chosen and subject
	// to no strength rule on purpose, so this is the one real brute-force
	// target on the page. The throttle makes each attempt wait out what the
	// previous failures from this source earned -- before the check, so a
	// wrong guess cannot be compared and retried at full speed -- and it
	// never refuses: the right password still works, it just waits. See
	// throttle.go for why a lockout would have been a denial of service
	// anyone could trigger.
	// wait never refuses: an attempt that cannot get a sleeping slot skips
	// the delay and is checked here anyway, so a correct password works
	// under any load. The only error it returns is a cancelled request.
	who := throttleKey(r.RemoteAddr)
	if err := s.throttle.wait(r.Context(), who); err != nil {
		// The client gave up while waiting, almost always. Answer
		// explicitly rather than falling out of the handler: returning
		// without writing makes net/http send an empty 200, and a browser
		// that is still there -- if this context were ever cancelled for
		// some other reason -- would read that as a successful sign-in.
		http.Error(w, "the sign-in was cancelled before it could be checked", http.StatusRequestTimeout)
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
