package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// TestApplyAllReportsEveryCameraSeparately is the brief's test verbatim.
//
// Eight cameras are eight independent outcomes and partial success is the
// normal case, not an error: the fisheye and the dual lens model disagree
// with the other six about most things. A single summary line over a
// partial apply is the same lie as reporting a 200 as proof.
func TestApplyAllReportsEveryCameraSeparately(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one", "two", "three"),
	})
	apply := func(ctx context.Context, cam Camera) (WriteResult, error) {
		if cam.Name == "two" {
			return WriteResult{}, errors.New("dial tcp: refused")
		}
		return WriteResult{Outcome: "confirmed"}, nil
	}

	got := s.applyAll(context.Background(), apply)
	if len(got) != 3 {
		t.Fatalf("got %d results, want one per camera", len(got))
	}
	by := map[string]FleetResult{}
	for _, r := range got {
		by[r.Camera] = r
	}
	if by["one"].Outcome != "confirmed" || by["three"].Outcome != "confirmed" {
		t.Fatalf("healthy cameras not confirmed: %+v", got)
	}
	if by["two"].Outcome == "confirmed" {
		t.Fatal("a camera that failed was reported as confirmed")
	}
	if by["two"].Detail == "" {
		t.Fatal("the failing camera gave no reason")
	}
}

// TestApplyAllDoesNotStopAtTheFirstFailure is the brief's test verbatim.
//
// A camera that is unreachable must not prevent the rest being set.
func TestApplyAllDoesNotStopAtTheFirstFailure(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "first", "second"),
	})
	// Guarded: applyAll runs the cameras concurrently, so the record of
	// which ones it tried is written from several goroutines at once.
	var mu sync.Mutex
	var tried []string
	apply := func(ctx context.Context, cam Camera) (WriteResult, error) {
		mu.Lock()
		tried = append(tried, cam.Name)
		mu.Unlock()
		return WriteResult{}, errors.New("unreachable")
	}

	got := s.applyAll(context.Background(), apply)
	mu.Lock()
	defer mu.Unlock()
	if len(tried) != 2 {
		t.Fatalf("tried %v, want both cameras attempted", tried)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
}

// TestApplyAllBoundsEachCameraSeparately proves the timeout is per camera,
// not shared off one deadline: one dead camera must not eat the budget for
// everyone else in the fleet.
//
// Checking only that each call's ctx.Deadline() reports ok is not enough: an
// implementation that hoists a single context.WithTimeout above the fan-out
// and hands it to every camera would pass that check too, since every call
// still sees a deadline, just the same one.
//
// The tell is that each camera is handed its OWN context. An earlier version
// of this test compared deadline values instead, which worked only while
// applyAll ran the cameras one after another -- two contexts created in the
// same instant may legitimately carry the same deadline once they are
// created concurrently, and the shared slice it recorded them in was itself
// a data race. Comparing the contexts themselves is exact, needs no timing
// margin, and says what the property actually is.
func TestApplyAllBoundsEachCameraSeparately(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "first", "second"),
	})

	var mu sync.Mutex
	seen := make([]context.Context, 0, 2)
	missingDeadline := make([]string, 0, 2)
	apply := func(ctx context.Context, cam Camera) (WriteResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := ctx.Deadline(); !ok {
			missingDeadline = append(missingDeadline, cam.Name)
		}
		seen = append(seen, ctx)
		return WriteResult{Outcome: "confirmed"}, nil
	}

	s.applyAll(context.Background(), apply)

	mu.Lock()
	defer mu.Unlock()
	if len(missingDeadline) > 0 {
		t.Fatalf("cameras ran with no deadline: %v; each must be bounded on its own", missingDeadline)
	}
	if len(seen) != 2 {
		t.Fatalf("got %d calls, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Fatal("both cameras were handed the same context: the timeout is shared across the fleet rather than given fresh per camera")
	}
}

// fakeClockCamera is a CGI camera for the fleet-apply path: it answers
// GetNtp/SetNtp exactly as fakeNTPCamera in time_test.go does, plus
// GetTime/SetTime for the timezone path time.go adds, and it records
// what each write actually received so a test can check the camera's own
// state rather than trust the handler's claim.
type fakeClockCamera struct {
	srv      *httptest.Server
	enable   int32
	server   atomic.Value // string
	timeZone int32
	ntpSets  int32
	timeSets int32
}

func newFakeClockCamera(t *testing.T, enable int, server string, timeZone int) *fakeClockCamera {
	t.Helper()
	f := &fakeClockCamera{enable: int32(enable), timeZone: int32(timeZone)}
	f.server.Store(server)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "Login":
			w.Write([]byte(loginReply("tok")))
		case "GetNtp":
			enable := atomic.LoadInt32(&f.enable)
			server := f.server.Load().(string)
			w.Write([]byte(`[{"cmd":"GetNtp","code":0,"value":{"Ntp":{"enable":` +
				strconv.Itoa(int(enable)) + `,"server":"` + server + `","port":123,"interval":1440}}}]`))
		case "SetNtp":
			body, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					Ntp struct {
						Enable int    `json:"enable"`
						Server string `json:"server"`
					} `json:"Ntp"`
				} `json:"param"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad SetNtp body: %v", err)
			} else if len(req) == 1 {
				atomic.StoreInt32(&f.enable, int32(req[0].Param.Ntp.Enable))
				f.server.Store(req[0].Param.Ntp.Server)
			}
			atomic.AddInt32(&f.ntpSets, 1)
			w.Write([]byte(`[{"cmd":"SetNtp","code":0}]`))
		case "GetTime":
			tz := atomic.LoadInt32(&f.timeZone)
			w.Write([]byte(`[{"cmd":"GetTime","code":0,"value":{"Time":{"timeZone":` + strconv.Itoa(int(tz)) + `,"year":2026}}}]`))
		case "SetTime":
			body, _ := io.ReadAll(r.Body)
			var req []struct {
				Param struct {
					Time struct {
						TimeZone int `json:"timeZone"`
						Year     int `json:"year"`
					} `json:"Time"`
				} `json:"param"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad SetTime body: %v", err)
			} else if len(req) == 1 {
				if req[0].Param.Time.Year != 2026 {
					t.Errorf("SetTime dropped the year field it was never asked to change: got %d", req[0].Param.Time.Year)
				}
				atomic.StoreInt32(&f.timeZone, int32(req[0].Param.Time.TimeZone))
			}
			atomic.AddInt32(&f.timeSets, 1)
			w.Write([]byte(`[{"cmd":"SetTime","code":0}]`))
		default:
			t.Errorf("unexpected cmd %q", r.URL.Query().Get("cmd"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClockCamera) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// twoClockCameras wires a two-camera fleet ("one", "two") to two fake CGI
// cameras and returns the server plus both fakes, which is the setup every
// every-camera test below shares.
func twoClockCameras(t *testing.T) (*httptest.Server, *fakeClockCamera, *fakeClockCamera) {
	t.Helper()
	one := newFakeClockCamera(t, 0, "old.example", 0)
	two := newFakeClockCamera(t, 0, "old.example", 0)
	cgiDial := func(cam Camera) (*cgi.Client, error) {
		switch cam.Name {
		case "one":
			return cgi.Dial(one.addr(), "admin", "")
		case "two":
			return cgi.Dial(two.addr(), "admin", "")
		}
		t.Fatalf("unexpected camera %q", cam.Name)
		return nil, nil
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one", "two"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, one, two
}

// resultRow is the table cell an every-camera result renders for one
// camera. Tests match on this rather than on the bare camera name: every
// page draws the whole fleet in its sidebar, so "two" appears on this page
// whether or not camera two was written, and a test that matched the name
// alone would pass against a handler that wrote nothing.
func resultRow(camera, outcome string) string {
	return "<td>" + camera + `</td><td class="outcome-` + outcome + `">`
}

// TestTheTimeFormWritesOnlyThisCameraByDefault is the default the fleet
// apply page never offered: the box is unticked, so the NTP form writes the
// camera whose page it is and no other.
//
// It asserts camera two's state, not just camera one's: "did not write the
// rest of the fleet" is the claim, and only the other camera can support
// it.
func TestTheTimeFormWritesOnlyThisCameraByDefault(t *testing.T) {
	ts, one, two := twoClockCameras(t)

	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/one/time", url.Values{"server": {"time.nist.gov"}, "enabled": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303 back to the camera's own page: %s", resp.StatusCode, raw)
	}
	if got := one.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera one holds server=%q, want time.nist.gov", got)
	}
	if got := two.server.Load().(string); got != "old.example" {
		t.Fatalf("camera two holds server=%q: an unticked box wrote the rest of the fleet", got)
	}
	if got := atomic.LoadInt32(&two.ntpSets); got != 0 {
		t.Fatalf("camera two received %d SetNtp calls, want none", got)
	}
}

// TestTickingEveryCameraWritesTheWholeFleetAndReportsEachOne is the
// capability folded in from the deleted /fleet/apply page: the same NTP
// form, ticked, reaches every camera and renders one row each.
func TestTickingEveryCameraWritesTheWholeFleetAndReportsEachOne(t *testing.T) {
	ts, one, two := twoClockCameras(t)

	resp, err := http.PostForm(ts.URL+"/cameras/one/time", url.Values{
		"server": {"time.nist.gov"}, "enabled": {"1"}, "every": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	html := string(raw)
	for _, want := range []string{resultRow("one", "confirmed"), resultRow("two", "confirmed")} {
		if !strings.Contains(html, want) {
			t.Errorf("page has no result row %q:\n%s", want, html)
		}
	}
	if got := one.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera one holds server=%q, want time.nist.gov", got)
	}
	if got := two.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera two holds server=%q, want time.nist.gov", got)
	}
}

// TestTickingEveryCameraWritesTheTimezoneToTheWholeFleet is the same proof
// for the other setting the deleted page carried. The timezone form is a
// write now, not the read-only display it used to be on this page.
func TestTickingEveryCameraWritesTheTimezoneToTheWholeFleet(t *testing.T) {
	ts, one, two := twoClockCameras(t)

	resp, err := http.PostForm(ts.URL+"/cameras/one/timezone", url.Values{
		"timezone": {"-18000"}, "every": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	html := string(raw)
	for _, want := range []string{resultRow("one", "confirmed"), resultRow("two", "confirmed")} {
		if !strings.Contains(html, want) {
			t.Errorf("page has no result row %q:\n%s", want, html)
		}
	}
	if got := atomic.LoadInt32(&one.timeZone); got != -18000 {
		t.Fatalf("camera one holds timeZone=%d, want -18000", got)
	}
	if got := atomic.LoadInt32(&two.timeZone); got != -18000 {
		t.Fatalf("camera two holds timeZone=%d, want -18000", got)
	}
}

// TestTheTimezoneFormWritesOnlyThisCameraByDefault pins the default on the
// timezone form too: one form having a safe default and the other not would
// be worse than neither having one, because the operator would learn the
// wrong rule from whichever they used first.
func TestTheTimezoneFormWritesOnlyThisCameraByDefault(t *testing.T) {
	ts, one, two := twoClockCameras(t)

	c := flashBrowser(t)
	resp := postForm(t, c, ts.URL+"/cameras/one/timezone", url.Values{"timezone": {"-18000"}})
	if resp.StatusCode != http.StatusSeeOther {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 303 back to the camera's own page: %s", resp.StatusCode, raw)
	}
	if got := atomic.LoadInt32(&one.timeZone); got != -18000 {
		t.Fatalf("camera one holds timeZone=%d, want -18000", got)
	}
	if got := atomic.LoadInt32(&two.timeSets); got != 0 {
		t.Fatalf("camera two received %d SetTime calls, want none", got)
	}

	page := getPage(t, c, ts.URL+"/cameras/one/time")
	if !strings.Contains(page, "one: timezone saved") {
		t.Fatalf("the banner does not say what was saved:\n%s", page)
	}
}

// TestOneCameraFailingDoesNotStopOrHideTheOthers is the partial apply this
// hardware actually produces: eight cameras of three models, and the
// fisheye disagreeing with the rest is the ordinary case. The camera that
// failed must be on the page, named, with its reason, and the cameras that
// worked must still have been written.
func TestOneCameraFailingDoesNotStopOrHideTheOthers(t *testing.T) {
	one := newFakeClockCamera(t, 0, "old.example", 0)
	three := newFakeClockCamera(t, 0, "old.example", 0)
	// Camera "two" answers nothing: cgiDial fails for it the way a
	// powered-off or rebooting camera fails, before any request is made.
	cgiDial := func(cam Camera) (*cgi.Client, error) {
		switch cam.Name {
		case "one":
			return cgi.Dial(one.addr(), "admin", "")
		case "two":
			return nil, errors.New("dial tcp 192.0.2.11:80: connect: no route to host")
		case "three":
			return cgi.Dial(three.addr(), "admin", "")
		}
		t.Fatalf("unexpected camera %q", cam.Name)
		return nil, nil
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one", "two", "three"), CGIDial: cgiDial})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.PostForm(ts.URL+"/cameras/one/time", url.Values{
		"server": {"time.nist.gov"}, "enabled": {"1"}, "every": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	html := string(raw)
	if !strings.Contains(html, resultRow("two", "refused")) {
		t.Errorf("the camera that failed is not reported as refused:\n%s", html)
	}
	if !strings.Contains(html, "no route to host") {
		t.Errorf("the refusal carries no reason:\n%s", html)
	}
	// The camera AFTER the failure matters most: a loop that aborted on
	// the first error would leave this one untouched and unreported.
	if !strings.Contains(html, resultRow("three", "confirmed")) {
		t.Errorf("the camera after the failing one is not reported:\n%s", html)
	}
	if got := three.server.Load().(string); got != "time.nist.gov" {
		t.Fatalf("camera three holds server=%q: a failure earlier in the fleet stopped the apply", got)
	}
}

// TestSetTimeZonePreservesEveryOtherField proves setTimeZone changes only
// timeZone on top of the camera's current document rather than inventing a
// document with the rest of GetTime's fields zeroed. fakeClockCamera's
// SetTime handler fails the test itself if "year" arrives as anything but
// what GetTime reported, so this is checked on the wire, not just on the
// handler's return value.
func TestSetTimeZonePreservesEveryOtherField(t *testing.T) {
	cam := newFakeClockCamera(t, 0, "pool.ntp.org", -28800)
	cgiDial := func(c Camera) (*cgi.Client, error) {
		return cgi.Dial(cam.addr(), "admin", "")
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), CGIDial: cgiDial})

	camera, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.setTimeZone(context.Background(), camera, -18000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "confirmed" {
		t.Fatalf("outcome = %q, want confirmed: %s", result.Outcome, result.Detail)
	}
	if atomic.LoadInt32(&cam.timeZone) != -18000 {
		t.Fatalf("camera holds timeZone=%d, want -18000", cam.timeZone)
	}
	if atomic.LoadInt32(&cam.timeSets) != 1 {
		t.Fatalf("camera received %d SetTime calls, want 1", cam.timeSets)
	}
}
