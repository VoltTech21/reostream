package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// RequireLogin puts HTTP Basic authentication in front of next, with the
// same login the RTSP listener asks for, so a recorder holds one login for
// reostream whichever output it reads. An empty user returns next as it is:
// the streaming port has always been open, and stays open unless a login is
// configured.
//
// /api/status and /metrics stay open. They carry no picture, and they are
// what a container health check and a Prometheus scrape point at, neither
// of which should need a recorder's password to say the daemon is up.
func RequireLogin(next http.Handler, user, pass string) http.Handler {
	if user == "" {
		return next
	}
	// Hashed first so the comparison takes the same time whatever the
	// lengths, which ConstantTimeCompare alone does not promise.
	wantUser := sha256.Sum256([]byte(user))
	wantPass := sha256.Sum256([]byte(pass))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		// Both compared every time, so a right username with a wrong
		// password takes no longer than a wrong username.
		userOK := subtle.ConstantTimeCompare(gotUser[:], wantUser[:])
		passOK := subtle.ConstantTimeCompare(gotPass[:], wantPass[:])
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="reostream", charset="UTF-8"`)
			http.Error(w, "this stream needs the [rtsp] username and password", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
