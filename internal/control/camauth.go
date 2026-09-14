package control

import (
	"net/http"

	"github.com/VoltTech21/reostream/internal/webui"
)

func (s *CameraServer) serveLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "cameralogin.html", struct {
		Title  string
		Failed bool
	}{Title: "Sign in"})
}

func (s *CameraServer) serveLogin(w http.ResponseWriter, r *http.Request) {
	got := r.FormValue("password")
	if !s.auth.Check(got) {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "cameralogin.html", struct {
			Title  string
			Failed bool
		}{Title: "Sign in", Failed: true})
		return
	}
	tok, err := s.sessions.Issue()
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.auth.CookieName,
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
