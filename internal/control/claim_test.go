package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMayClaimOnlyFromAPrivateAddress(t *testing.T) {
	// Claim on first visit means the first person to arrive owns the
	// install, and this one can write to cameras. A private source keeps
	// that to whoever is already on the network; a public one means the
	// port is exposed and a stranger could take it.
	cases := []struct {
		addr string
		want bool
	}{
		{"192.168.1.10:5000", true},
		{"10.0.0.5:5000", true},
		{"172.16.4.1:5000", true},
		{"127.0.0.1:5000", true},
		{"[::1]:5000", true},
		{"[fd00::1]:5000", true},
		{"8.8.8.8:5000", false},
		{"[2606:4700::1111]:5000", false},
		// Carrier-grade NAT, where every Tailscale address lives and where
		// an ISP's shared NAT also lives. Refused on purpose: an address
		// out of this range cannot say which of the two it is. See
		// mayClaim's comment before widening it.
		{"100.101.102.103:5000", false},
		// Unparseable sources fail closed rather than open.
		{"not-an-address", false},
		{"", false},
		{"192.168.1.10", false},
	}
	for _, tc := range cases {
		if got := mayClaim(tc.addr); got != tc.want {
			t.Errorf("mayClaim(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestAnUnclaimedInstallServesTheClaimScreen(t *testing.T) {
	s := newTestServer(t, Options{ConfigPath: filepath.Join(t.TempDir(), "config.toml")})
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/claim") {
		t.Fatalf("unclaimed install returned %d to %q, want a redirect to /claim",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestAClaimedInstallDoesNotOfferTheClaimScreen(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	req := httptest.NewRequest("GET", "/claim", nil)
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("a claimed install still served the claim screen")
	}
}

func TestClaimingFromAPublicAddressIsRefused(t *testing.T) {
	s := newTestServer(t, Options{ConfigPath: filepath.Join(t.TempDir(), "config.toml")})
	req := httptest.NewRequest("POST", "/claim", strings.NewReader("password=hunter2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "8.8.8.8:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a public address was allowed to claim the install")
	}
}

// postClaim submits a claim from a LAN address.
func postClaim(t *testing.T, s *Server, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/claim", strings.NewReader("password="+password))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getFrom fetches path as if from a LAN browser with no session.
func getFrom(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The point of the whole claim: the running process must start requiring
// the new password immediately. A claim that only took effect at the next
// restart would leave a "claimed" install serving unauthenticated, which is
// exactly what the claim exists to prevent.
func TestClaimingTakesEffectWithoutARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// AllowNoPassword, as the synthesized first-run config sets it: this
	// is the state a fresh daemon is really in.
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})

	if rec := getFrom(t, s, "/claim"); rec.Code != http.StatusOK {
		t.Fatalf("unclaimed install answered %d for the claim screen, want 200 with no password", rec.Code)
	}

	if rec := postClaim(t, s, "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("claim returned %d, want 303; body: %s", rec.Code, rec.Body.String())
	}

	rec := getFrom(t, s, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("after claiming, / answered %d to %q, want 303 to /login: the running server is still unauthenticated",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec := getFrom(t, s, "/claim"); rec.Code != http.StatusNotFound {
		t.Fatalf("after claiming, /claim answered %d, want 404", rec.Code)
	}

	// And the login route checks the new password, not the empty one it
	// was constructed with.
	if !s.authNow().Check("correct-horse") {
		t.Fatal("the live auth state does not hold the claimed password")
	}
}

func TestClaimWritesTheConfigItPromises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if rec := postClaim(t, s, "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("claim returned %d; body: %s", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("claim did not leave a config behind: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "[control]") || !strings.Contains(text, `password = "correct-horse"`) {
		t.Fatalf("claimed config has no [control] password:\n%s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file holds a password, so it must not be readable by anyone else.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("claimed config is mode %04o, want no group or other access", perm)
	}
}

func TestAnAlreadyClaimedInstallCannotBeClaimedAgain(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	if rec := postClaim(t, s, "attacker"); rec.Code != http.StatusNotFound {
		t.Fatalf("a claimed install answered %d to a second claim, want 404", rec.Code)
	}
	// An install configured by hand to run without a password is claimed
	// too: that config is somebody's deliberate choice, and a claim would
	// overwrite it.
	byHand := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "one")})
	if rec := postClaim(t, byHand, "attacker"); rec.Code != http.StatusNotFound {
		t.Fatalf("an install with a hand-written config answered %d to a claim, want 404", rec.Code)
	}
}

// A claim is the one state-changing route with no session behind it, so a
// cross-site POST is the way to steal one: the victim's browser reaches the
// LAN address the gate is happy with, and the attacker picks the password.
func TestACrossSiteClaimIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	req := httptest.NewRequest("POST", "/claim", strings.NewReader("password=attacker"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-site claim answered %d, want 403", rec.Code)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a cross-site claim wrote a config")
	}
	if s.claimed() {
		t.Fatal("a cross-site claim claimed the install")
	}
}

func TestAClaimFormFromThisPageIsAccepted(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: filepath.Join(t.TempDir(), "config.toml")})
	req := httptest.NewRequest("POST", "/claim", strings.NewReader("password=correct-horse"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a same-origin claim answered %d, want 303", rec.Code)
	}
}

func TestAPasswordTheConfigCannotHoldIsRefused(t *testing.T) {
	for _, pw := range []string{"", "$SECRET", "two\nlines"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
		rec := postClaim(t, s, pw)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("password %q was accepted, want a refusal", pw)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("password %q was refused but a config was written anyway", pw)
		}
	}
}

// The claim password is a secret from the moment it is typed: it must not
// turn up in what the page says back, nor in the URL it redirects to.
func TestAClaimNeverEchoesThePassword(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: filepath.Join(t.TempDir(), "config.toml")})
	rec := postClaim(t, s, "correct-horse")
	if strings.Contains(rec.Body.String(), "correct-horse") ||
		strings.Contains(rec.Header().Get("Location"), "correct-horse") {
		t.Fatal("the claim response carried the password back")
	}
}

// A refusal has to be actionable: a bare 403 on a first run is a dead end
// for somebody who is not going to read the source.
func TestARefusedClaimSaysWhatToDoInstead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	req := httptest.NewRequest("GET", "/claim", nil)
	// A tailnet address: the operator's own machine, refused all the same.
	req.RemoteAddr = "100.101.102.103:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"100.101.102.103", "local network", path} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `type="password"`) {
		t.Error("the refusal still offers a password form that cannot be submitted")
	}
}

// Routes are read by many goroutines while the claim handler writes the
// auth state. Run under -race; a field mutation without the lock fails here.
func TestClaimIsRaceFreeAgainstConcurrentRequests(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: filepath.Join(t.TempDir(), "config.toml")})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				getFrom(t, s, "/")
			}
		}()
	}
	if rec := postClaim(t, s, "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Errorf("claim returned %d", rec.Code)
	}
	close(stop)
	wg.Wait()
}
