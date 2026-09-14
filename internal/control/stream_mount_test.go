package control_test

import (
	"net/http"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
)

// TestStreamMountServesAHubSameOrigin is the fix for the tiles that could
// never play: the control listener must serve stream routes itself, so a
// browser fetching a tile's URL never crosses an origin the streaming
// listener does not, and must not, allow.
func TestStreamMountServesAHubSameOrigin(t *testing.T) {
	hubs := server.StaticHubs(map[string]*hub.Hub{"gate/main": hub.New(4)})
	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "gate"), Hubs: hubs})

	resp, err := http.Head(ts.URL + "/stream/gate.ts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /stream/gate.ts = %d, want 200", resp.StatusCode)
	}
}

// TestStreamMountRequiresAuth proves the stream mount sits behind the same
// authed wrapper as the rest of the page: this listener holds camera
// credentials, unlike the streaming listener, so it cannot serve video
// without a session just because the path happens to look like the other
// listener's.
func TestStreamMountRequiresAuth(t *testing.T) {
	hubs := server.StaticHubs(map[string]*hub.Hub{"gate/main": hub.New(4)})
	ts := newTestServer(t, control.Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "gate"), Hubs: hubs})

	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Get(ts.URL + "/stream/gate.ts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /stream/gate.ts = %d, want a redirect to login", resp.StatusCode)
	}
}

// TestStreamMountAbsentWithNoHubs proves a control server with no Hubs
// wired up (a config a test builds, or one variant of an unfinished daemon
// wiring) never panics on the route: the mount is only added when Hubs is
// non-nil, and the route otherwise simply does not exist.
func TestStreamMountAbsentWithNoHubs(t *testing.T) {
	ts := newTestServer(t, control.Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "gate")})

	resp, err := http.Head(ts.URL + "/stream/gate.ts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404 with no Hubs wired up", resp.StatusCode)
	}
}
