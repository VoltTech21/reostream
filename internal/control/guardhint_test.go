package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// With no guard position saved, the hint points at this page's own button,
// not at the Reolink app (fake hardware report, 10-30, bug 8).
func TestGuardHintPointsAtThePagesOwnButton(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/cameras/one/settings/ptz", nil), "camera.html",
		cameraPage{Title: "one", Camera: Camera{Name: "one"}, Cat: "ptz"})
	body := w.Body.String()
	if strings.Contains(body, "set one in the Reolink app") {
		t.Errorf("the guard hint still sends people to the Reolink app:\n%s", body)
	}
	if !strings.Contains(body, "No guard position set yet") || !strings.Contains(body, "Set to where it points now") {
		t.Errorf("the guard hint or its button is missing:\n%s", body)
	}
}
