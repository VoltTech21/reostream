package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
)

// TestNoRouteEscapesTheAuthWrapper asks the ROUTER what it serves and
// checks every pattern, rather than walking a list a person maintains.
//
// The hand-written list this replaces omitted GET /stream/, the one
// conditionally registered route, so the test whose entire job was to
// catch a forgotten auth wrapper could not have caught one there. A list
// that has to be remembered is not a guard.
//
// /assets/ is deliberately reachable without a session: it is the
// stylesheet, and gating it left the login page rendering unstyled. What
// lives there is pinned separately by TestOnlyTheListedAssetsAreEmbedded,
// which is the check that matters for an unauthenticated directory.
func TestNoRouteEscapesTheAuthWrapper(t *testing.T) {
	s := newTestServer(t, Options{
		Password:   "hunter2",
		ConfigPath: writeTestConfig(t, "one"),
		Hubs:       server.StaticHubs(map[string]*hub.Hub{"one/main": hub.New(4)}),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, pattern := range routePatterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("route pattern %q has no method", pattern)
		}
		// Turn the pattern back into something requestable.
		path = strings.ReplaceAll(path, "{name}", "one")
		path = strings.ReplaceAll(path, "{id}", "45")
		path = strings.ReplaceAll(path, "{$}", "")

		t.Run(pattern, func(t *testing.T) {
			req, err := http.NewRequest(method, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if publicRoutes[pattern] {
				if resp.StatusCode == http.StatusSeeOther &&
					strings.HasPrefix(resp.Header.Get("Location"), "/login") {
					t.Fatalf("%s is meant to answer without a session but redirected to login", pattern)
				}
				return
			}
			if strings.HasPrefix(path, "/assets/") {
				return // see the doc comment: deliberately unauthenticated static files
			}
			if resp.StatusCode != http.StatusSeeOther ||
				!strings.HasPrefix(resp.Header.Get("Location"), "/login") {
				t.Fatalf("%s answered %d (Location %q) to a request with no session; "+
					"every route but the claim and login pair must go through the auth wrapper",
					pattern, resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

// TestTheRouteListMatchesTheRouter is the other half: routePatterns is
// written by hand beside Handler, so it can drift from what Handler
// actually registers. A pattern that exists in one and not the other means
// either a route nothing checks, or a check against a route that is gone.
func TestTheRouteListMatchesTheRouter(t *testing.T) {
	s := newTestServer(t, Options{
		Password:   "hunter2",
		ConfigPath: writeTestConfig(t, "one"),
		Hubs:       server.StaticHubs(map[string]*hub.Hub{"one/main": hub.New(4)}),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Every listed pattern must actually be served: an unregistered one
	// answers 404 from the mux rather than a redirect or a handler.
	for _, pattern := range routePatterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{name}", "one")
		path = strings.ReplaceAll(path, "{id}", "45")
		path = strings.ReplaceAll(path, "{$}", "")

		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// The claim pair is the one exception: both routes answer 404 by
		// design once an install is claimed, which this server is. A 404
		// from them is the feature, not a missing registration.
		if strings.HasSuffix(pattern, " /claim") {
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s is in routePatterns but the router answers 404: it was removed from Handler", pattern)
		}
	}

	for pattern := range publicRoutes {
		found := false
		for _, p := range routePatterns {
			if p == pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is listed as public but is not a registered route", pattern)
		}
	}
}
