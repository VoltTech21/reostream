package control

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test in this file sweeps 127.0.0.0/24 and nothing else. The
// network this repository is developed on carries a live reostream, an
// NVR and eight real cameras, and a test that swept a real range would
// open a connection to every one of them. Loopback is the only range these
// may touch, which is also why parseSweepRange accepts loopback at all.
const loopbackRange = "127.0.0.0/24"

// listenLoopback binds a plain TCP listener on the given loopback host,
// with a port the kernel picks, and returns that port. A camera on this
// sweep's terms is just an address that accepts a connection on the
// Baichuan port, so a bare listener is a faithful stand-in: the sweep
// writes no byte and reads no byte, it only asks whether the dial
// succeeded.
func listenLoopback(t *testing.T, host string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	t.Cleanup(func() { ln.Close() })
	// Accept and drop, so the sweep's connections complete rather than
	// piling up in the backlog.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

// stubReport is a Probe that never dials anything: it reports a fixed
// model for whatever address it is handed. Options.Probe exists precisely
// so a test can do this.
func stubReport(model string) func(context.Context, string, string, string) CameraReport {
	return func(_ context.Context, addr, _, _ string) CameraReport {
		return CameraReport{
			Model:   model,
			Streams: []StreamReport{{Name: "main", Codec: "h265", Width: 2560, Height: 1920}},
		}
	}
}

// writeDiscoverConfig writes a claimed config naming one camera at addr,
// so a test can assert an already-configured address is left alone.
func writeDiscoverConfig(t *testing.T, name, addr string) string {
	t.Helper()
	text := fmt.Sprintf("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\nallow_no_password = true\n\n"+
		"[[camera]]\nname = %q\naddress = %q\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n", name, addr)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A listener inside the swept range is found, and an address with nothing
// on it is not. This is the whole premise: the sweep's only claim about an
// address is whether something accepted a connection on the Baichuan port.
func TestASweepFindsAListenerAndSkipsSilentAddresses(t *testing.T) {
	port := listenLoopback(t, "127.0.0.2")
	s := newTestServer(t, Options{})
	s.sweepPort = port

	p, err := parseSweepRange(loopbackRange)
	if err != nil {
		t.Fatal(err)
	}
	live := s.sweepRange(context.Background(), p)

	found := false
	for _, addr := range live {
		if addr == "127.0.0.2" {
			found = true
		}
		// 127.0.0.77 has no listener on this port. Asserting over the
		// whole result rather than just one address is what catches a
		// sweep that reports every address as alive, which a dial with a
		// swallowed error would do.
		if addr == "127.0.0.77" {
			t.Errorf("127.0.0.77 has nothing listening on port %d but the sweep reported it", port)
		}
	}
	if !found {
		t.Fatalf("the sweep of %s did not find the listener on 127.0.0.2:%d; it found %v", loopbackRange, port, live)
	}
}

// An address already in the config is counted and then left alone: not
// probed, and not offered as something to add. Probing it is what
// probeGuarded refuses for its own reasons; offering it would invite an
// operator to add the same camera twice under two names, which is two
// daemons' worth of sessions against one camera.
func TestACameraAlreadyInTheConfigIsNotOfferedAgain(t *testing.T) {
	port := listenLoopback(t, "127.0.0.2")
	probed := make(chan string, 4)
	s := newTestServer(t, Options{
		ConfigPath: writeDiscoverConfig(t, "already-here", "127.0.0.2"),
		Probe: func(ctx context.Context, addr, user, pass string) CameraReport {
			probed <- addr
			return CameraReport{Model: "should not have been asked"}
		},
	})
	s.sweepPort = port

	p, err := parseSweepRange(loopbackRange)
	if err != nil {
		t.Fatal(err)
	}
	res := s.discover(context.Background(), p, "admin", "hunter2")

	if res.Configured != 1 {
		t.Errorf("Configured = %d, want 1: 127.0.0.2 answered and is already in the config", res.Configured)
	}
	for _, f := range res.Found {
		if f.Address == "127.0.0.2" {
			t.Errorf("127.0.0.2 is already configured but was offered again: %+v", f)
		}
	}
	select {
	case addr := <-probed:
		t.Errorf("the probe was run against %q, which is already configured", addr)
	default:
	}
}

// A candidate that is NOT configured goes through the existing probe path,
// and what that probe reported reaches the result. There must not be a
// second probe implementation here.
func TestAnUnconfiguredCandidateGoesThroughTheExistingProbe(t *testing.T) {
	port := listenLoopback(t, "127.0.0.3")
	s := newTestServer(t, Options{
		ConfigPath: writeDiscoverConfig(t, "elsewhere", "192.0.2.99"),
		Probe:      stubReport("IPC, 2560*1920, 1 channels"),
	})
	s.sweepPort = port

	p, err := parseSweepRange(loopbackRange)
	if err != nil {
		t.Fatal(err)
	}
	res := s.discover(context.Background(), p, "admin", "hunter2")

	for _, f := range res.Found {
		if f.Address != "127.0.0.3" {
			continue
		}
		if f.Report.Model != "IPC, 2560*1920, 1 channels" {
			t.Fatalf("Report.Model = %q, want what the probe reported", f.Report.Model)
		}
		return
	}
	t.Fatalf("127.0.0.3 was listening but is not in the result: %+v", res.Found)
}

// A cancelled request stops the sweep, and leaves nothing running behind
// it. A person who clicks and navigates away must not leave 254 dials in
// flight against their own network.
func TestASweepStopsWhenItsContextIsCancelled(t *testing.T) {
	port := listenLoopback(t, "127.0.0.2")
	s := newTestServer(t, Options{})
	s.sweepPort = port

	p, err := parseSweepRange(loopbackRange)
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan []string, 1)
	go func() { done <- s.sweepRange(ctx, p) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a sweep on a cancelled context did not return; it is not cancellable")
	}

	// sweepRange does not return until its own wg.Wait does, so by the
	// time it has returned there is no dialling goroutine left. Give the
	// runtime a moment to reap them before counting, since a goroutine
	// that has returned is not instantly gone from the count.
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines went from %d to %d across a cancelled sweep; dials were left running", before, runtime.NumGoroutine())
}

// The range is an instruction to open connections that somebody typed into
// a form, so anything but a private or loopback range is refused. Without
// this, a daemon reachable from anywhere is a port scanner for hire.
func TestANonPrivateRangeIsRefused(t *testing.T) {
	refused := []string{
		"8.8.8.0/24",     // public
		"203.0.113.0/24", // documentation range, still public
		"100.64.0.0/24",  // carrier-grade NAT: not RFC1918, not ours
		"10.0.0.0/16", // private but far too large
		"10.0.0.0/8",     // likewise
		"::1/128",        // not IPv4
		"not-a-range",    // not a range at all
		"192.168.1.50",   // an address, with no prefix
	}
	for _, r := range refused {
		t.Run(r, func(t *testing.T) {
			if _, err := parseSweepRange(r); err == nil {
				t.Fatalf("%s was accepted; only a private or loopback /24 or smaller may be swept", r)
			}
		})
	}

	accepted := []string{"192.168.1.0/24", "10.4.5.0/24", "172.16.9.0/25", "127.0.0.0/24"}
	for _, r := range accepted {
		t.Run(r, func(t *testing.T) {
			if _, err := parseSweepRange(r); err != nil {
				t.Fatalf("%s was refused (%v); it is a private or loopback range of an acceptable size", r, err)
			}
		})
	}
}

// The refusal reaches the operator through the handler too, not only
// through the validator: a range that is refused must be told to the
// person who typed it, and must not start a sweep.
func TestTheHandlerRefusesANonPrivateRange(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	body := url.Values{"range": {"8.8.8.0/24"}}.Encode()
	req := httptest.NewRequest("POST", "/setup/discover", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200 with a refusal a person can read", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not a private network") {
		t.Fatalf("the page did not say why the range was refused: %s", rec.Body.String())
	}
}

// One sweep at a time, for the same reason inFlightProbes serialises
// probes: two tabs would put twice the dials on the same wire.
func TestTwoConcurrentSweepsDoNotBothRun(t *testing.T) {
	s := newTestServer(t, Options{})
	if !s.beginSweep() {
		t.Fatal("the first sweep should be able to claim the slot")
	}
	if s.beginSweep() {
		t.Fatal("a second concurrent sweep claimed the slot too")
	}
	s.endSweep()
	if !s.beginSweep() {
		t.Fatal("after endSweep the slot should be claimable again")
	}
}

// And through the handler: the second request must be told a sweep is
// already running rather than silently starting another one.
func TestTheHandlerRefusesASecondConcurrentSweep(t *testing.T) {
	port := listenLoopback(t, "127.0.0.2")
	release := make(chan struct{})
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Probe: func(context.Context, string, string, string) CameraReport {
			<-release // hold the first sweep open while the second arrives
			return CameraReport{Model: "held"}
		},
	})
	s.sweepPort = port

	post := func() *httptest.ResponseRecorder {
		body := url.Values{"range": {loopbackRange}}.Encode()
		req := httptest.NewRequest("POST", "/setup/discover", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		post()
	}()

	// Wait until the first sweep has actually claimed the slot, rather
	// than racing it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.inFlightMu.Lock()
		running := s.sweeping
		s.inFlightMu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatal("the first sweep never claimed the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := post()
	if !strings.Contains(second.Body.String(), "already running") {
		t.Errorf("the second sweep was not refused: %s", second.Body.String())
	}

	close(release)
	wg.Wait()
}

// No credential the sweep was given may come back out of it: not in the
// rendered page, and not in any field of the result. The password is used
// for the probe and is not the sweep's to keep.
func TestASweepNeverEchoesACredential(t *testing.T) {
	const password = "qv7-marker-password"
	port := listenLoopback(t, "127.0.0.2")
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeTestConfig(t, "one"),
		Probe:           stubReport("IPC"),
	})
	s.sweepPort = port

	body := url.Values{
		"range":    {loopbackRange},
		"username": {"admin"},
		"password": {password},
	}.Encode()
	req := httptest.NewRequest("POST", "/setup/discover", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), password) {
		t.Fatalf("the rendered sweep carries the password it was given:\n%s", rec.Body.String())
	}
	for _, v := range rec.Result().Header {
		for _, h := range v {
			if strings.Contains(h, password) {
				t.Fatalf("a response header carries the password: %q", h)
			}
		}
	}
}

// The range offered on the setup page is derived from this machine's own
// interfaces, and whatever it offers must itself be sweepable: a page that
// offers a range the handler then refuses is a dead button.
func TestTheOfferedRangesAreThemselvesAcceptable(t *testing.T) {
	for _, r := range localSweepRanges() {
		if _, err := parseSweepRange(r); err != nil {
			t.Errorf("the setup page offers %s but the sweep refuses it: %v", r, err)
		}
	}
}

// hostsIn drops the network and broadcast addresses, which nothing answers
// on, and keeps everything between.
func TestHostsInDropsNetworkAndBroadcast(t *testing.T) {
	p := netip.MustParsePrefix("192.168.1.0/24")
	hosts := hostsIn(p)
	if len(hosts) != 254 {
		t.Fatalf("a /24 yielded %d hosts, want 254", len(hosts))
	}
	if hosts[0].String() != "192.168.1.1" {
		t.Errorf("first host is %s, want 192.168.1.1", hosts[0])
	}
	if hosts[len(hosts)-1].String() != "192.168.1.254" {
		t.Errorf("last host is %s, want 192.168.1.254", hosts[len(hosts)-1])
	}
}
