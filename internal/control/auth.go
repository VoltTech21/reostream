package control

import (
	"errors"
	"net/http"

	"github.com/VoltTech21/reostream/internal/webui"
)

// loginPage is what login.html renders. Failed and Busy are separate
// because they are different instructions: one says type it again, the
// other says the page is under a flood and this attempt was not judged at
// all.
type loginPage struct {
	Title  string
	Failed bool
	Busy   string
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
	who := throttleKey(r.RemoteAddr)
	if err := s.throttle.wait(r.Context(), who); err != nil {
		if errors.Is(err, errThrottleBusy) {
			// A flood is already in progress and every waiting slot is
			// taken. Refusing now is the lesser of two evils: queueing is
			// what would turn the delay into the connection exhaustion the
			// cap exists to prevent.
			w.WriteHeader(http.StatusServiceUnavailable)
			s.render(w, "login.html", loginPage{
				Title: "Sign in",
				Busy:  "Too many sign-in attempts are arriving at once, so this one was not checked. Try again in a moment.",
			})
			return
		}
		// The client gave up while waiting. Nothing to write to.
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
