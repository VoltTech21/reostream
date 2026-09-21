package control

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// TestAnNTPWriteOffersAnUndoThatPostsTheFormsOwnFields is the property that
// was missing: the time page reported an NTP change and offered no way back,
// because WriteResult.Before is a sentence for a person to read ("on, server
// \"pool.ntp.org\"") rather than the pair of fields the form posts. A button
// sending that sentence back as a server name would have written nonsense to
// the camera, so none was offered at all.
//
// The pre-write read already happens -- setNTP does it to build Before -- so
// the pair was there to be kept. This checks undo carries the camera's actual
// previous server and enable state, in the form's own field names, back
// through the same handler that set them.
func TestAnNTPWriteOffersAnUndoThatPostsTheFormsOwnFields(t *testing.T) {
	cam := newFakeClockCamera(t, 1, "pool.ntp.org", -21600)
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "cam1"),
		CGIDial:         func(Camera) (*cgi.Client, error) { return cgi.Dial(cam.addr(), "admin", "") },
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := flashBrowser(t)

	postForm(t, c, ts.URL+"/cameras/cam1/time", url.Values{
		"server":  {"time.nist.gov"},
		"enabled": {"1"},
	})

	action, form := undoForm(t, getPage(t, c, ts.URL+"/cameras/cam1/time"))

	if want := "/cameras/cam1/time"; action != want {
		t.Fatalf("undo posts to %q, want the same handler that made the change, %q", action, want)
	}
	if got := form.Get("server"); got != "pool.ntp.org" {
		t.Fatalf("undo carries server %q, want the camera's own previous value", got)
	}
	if form.Get("enabled") != "1" {
		t.Fatalf("undo lost the previous enabled state: %v", form)
	}
	for k, v := range form {
		if strings.Contains(strings.Join(v, ""), `server "`) {
			t.Fatalf("undo carries the rendered sentence in %q rather than the field values", k)
		}
	}

	// And it must actually put the camera back when pressed.
	postForm(t, c, ts.URL+action, form)
	if got := cam.server.Load().(string); got != "pool.ntp.org" {
		t.Fatalf("after undo the camera holds %q, want its previous server", got)
	}
}

// TestUndoRestoresAnNTPThatWasOff covers the half a boolean makes easy to
// get wrong: restoring a camera that had NTP disabled must send it back to
// disabled, not merely restore the server name it was not using.
func TestUndoRestoresAnNTPThatWasOff(t *testing.T) {
	cam := newFakeClockCamera(t, 0, "pool.ntp.org", -21600)
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "cam1"),
		CGIDial:         func(Camera) (*cgi.Client, error) { return cgi.Dial(cam.addr(), "admin", "") },
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := flashBrowser(t)

	postForm(t, c, ts.URL+"/cameras/cam1/time", url.Values{
		"server": {"time.nist.gov"}, "enabled": {"1"},
	})
	action, form := undoForm(t, getPage(t, c, ts.URL+"/cameras/cam1/time"))
	if form.Get("enabled") != "" {
		t.Fatalf("undo would re-enable NTP on a camera that had it off: %v", form)
	}
	postForm(t, c, ts.URL+action, form)
	if atomic.LoadInt32(&cam.enable) != 0 {
		t.Fatal("after undo the camera has NTP on, but it was off before the write")
	}
}
