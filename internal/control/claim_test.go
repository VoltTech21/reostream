package control

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

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

// postClaim submits a claim carrying the token this server actually
// printed, which is what an operator reading the log has.
func postClaim(t *testing.T, s *Server, password string) *httptest.ResponseRecorder {
	t.Helper()
	return postClaimFrom(t, s, "192.168.1.10:5000", s.claimToken(), password)
}

// postClaimFrom submits a claim from a chosen source address with a chosen
// token, for the tests that care about one or the other.
func postClaimFrom(t *testing.T, s *Server, remoteAddr, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := url.Values{"token": {token}, "password": {password}}.Encode()
	req := httptest.NewRequest("POST", "/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
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

	if rec := postClaim(t, s, "correct-horse-battery"); rec.Code != http.StatusSeeOther {
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
	if !s.authNow().Check("correct-horse-battery") {
		t.Fatal("the live auth state does not hold the claimed password")
	}
}

func TestClaimWritesTheConfigItPromises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if rec := postClaim(t, s, "correct-horse-battery"); rec.Code != http.StatusSeeOther {
		t.Fatalf("claim returned %d; body: %s", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("claim did not leave a config behind: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "[control]") || !strings.Contains(text, `password = "correct-horse-battery"`) {
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
// cross-site POST is the way to steal one: the victim's browser reaches an
// address the attacker's own page cannot, and the attacker picks the
// password. The token blunts this but does not replace it -- CSRF is a
// separate concern from the claim gate -- so the check stays, and this test
// hands the forgery the real token to prove the check is doing the work.
func TestACrossSiteClaimIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	req := httptest.NewRequest("POST", "/claim",
		strings.NewReader(url.Values{"token": {s.claimToken()}, "password": {"attacker"}}.Encode()))
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
	req := httptest.NewRequest("POST", "/claim",
		strings.NewReader(url.Values{"token": {s.claimToken()}, "password": {"correct-horse-battery"}}.Encode()))
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
	rec := postClaim(t, s, "correct-horse-battery")
	if strings.Contains(rec.Body.String(), "correct-horse-battery") ||
		strings.Contains(rec.Header().Get("Location"), "correct-horse-battery") {
		t.Fatal("the claim response carried the password back")
	}
}

// writeHandConfig writes a config with a [control] section, the way an
// operator following the refusal's advice would, and returns its path.
func writeHandConfig(t *testing.T, control string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\n"+control+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The window the daemon's own advice opens: the refusal tells an operator
// to write a password into the config and THEN restart. Between those two
// steps the config exists, so the claim screen is gone and the first-run
// log is silent -- and this process must not still be serving every route
// with no password at all.
func TestAPasswordWrittenByHandIsRequiredWithoutARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})

	// Before: unclaimed, so the gate is shut and the claim screen is on.
	if rec := getFrom(t, s, "/"); rec.Header().Get("Location") != "/claim" {
		t.Fatalf("an install with no config sent / to %q, want /claim", rec.Header().Get("Location"))
	}

	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\npassword = \"by-hand\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := getFrom(t, s, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("with a hand-written password on disk, / answered %d to %q, want 303 to /login: the running server is still unauthenticated",
			rec.Code, rec.Header().Get("Location"))
	}
	if !s.authNow().Check("by-hand") {
		t.Fatal("the hand-written password was not adopted into the live auth state")
	}
	if s.authNow().AllowNoPassword {
		t.Fatal("the live auth state still allows no password")
	}
	if rec := getFrom(t, s, "/claim"); rec.Code != http.StatusNotFound {
		t.Fatalf("/claim answered %d on a hand-claimed install, want 404", rec.Code)
	}
}

// The shipped deployment writes password = "$REOSTREAM_CONTROL_PASSWORD"
// and sets the variable in the container, so adoption has to resolve one.
func TestAnAdoptedPasswordResolvesAnEnvironmentReference(t *testing.T) {
	t.Setenv("TEST_ADOPT_CONTROL_PW", "from-the-environment")
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeHandConfig(t, `password = "$TEST_ADOPT_CONTROL_PW"`),
	})
	if rec := getFrom(t, s, "/"); rec.Header().Get("Location") != "/login" {
		t.Fatalf("/ went to %q, want /login", rec.Header().Get("Location"))
	}
	if !s.authNow().Check("from-the-environment") {
		t.Fatal("the environment reference was not resolved before adoption")
	}
	if s.authNow().Check("$TEST_ADOPT_CONTROL_PW") {
		t.Fatal("the literal reference was adopted as the password")
	}
}

// An unset variable means this process cannot know the password. Serving on
// unauthenticated is not an option, so the page locks: refuse everybody
// until somebody fixes it. Locked and wrong is recoverable, open is not.
func TestAnUnresolvableAdoptedPasswordLocksThePage(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeHandConfig(t, `password = "$TEST_ADOPT_UNSET_PW"`),
	})
	// Every route refuses: the gate sends it to the claim screen, and the
	// claim screen has nothing to offer because the file is already there.
	if rec := getFrom(t, s, "/"); rec.Header().Get("Location") != "/claim" {
		t.Fatalf("/ went to %q, want /claim", rec.Header().Get("Location"))
	}
	auth := s.authNow()
	if auth.AllowNoPassword {
		t.Fatal("an unresolvable password left the page open")
	}
	for _, guess := range []string{"", "$TEST_ADOPT_UNSET_PW", "TEST_ADOPT_UNSET_PW"} {
		if auth.Check(guess) {
			t.Fatalf("the locked page accepted %q", guess)
		}
	}
}

// Locking is right; latching it is not. An operator who fixes the config by
// writing a real password must not need a restart to get in -- that was the
// whole point of adoption, and a lock that only a restart clears quietly
// takes it back.
func TestALockedPageHealsWhenARealPasswordIsWritten(t *testing.T) {
	path := writeHandConfig(t, `password = "$TEST_ADOPT_UNSET_PW_2"`)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})

	if rec := getFrom(t, s, "/"); rec.Header().Get("Location") != "/claim" {
		t.Fatalf("/ went to %q, want /claim", rec.Header().Get("Location"))
	}
	if s.claimed() {
		t.Fatal("a locked page counted as claimed, so nothing later can change its mind")
	}

	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\npassword = \"written-after\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := getFrom(t, s, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("after fixing the config, / answered %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if !s.authNow().Check("written-after") {
		t.Fatal("the password written after the lock was never adopted: the lock latched")
	}
}

// Rotating the password is what an operator does after a suspected
// compromise. A "Saved" banner over a page where the old credential still
// works would be the worst possible answer to it.
func TestRotatingThePasswordFromTheConfigPageTakesEffect(t *testing.T) {
	s := newTestServer(t, Options{
		Password:   "the-old-one",
		ConfigPath: writeHandConfig(t, `password = "the-old-one"`),
	})
	if _, err := s.writeAndApply("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\npassword = \"the-new-one\"\n"); err != nil {
		t.Fatal(err)
	}
	auth := s.authNow()
	if !auth.Check("the-new-one") {
		t.Fatal("the rotated password does not work")
	}
	if auth.Check("the-old-one") {
		t.Fatal("the password the operator believes they revoked still works")
	}
}

// A real secret is allowed to begin with a "$". config.Load has already
// resolved the file's "$NAME" reference by the time a save adopts it, so
// applying the reference rule a second time would look up the rest of the
// password as a variable name, find nothing, and lock the page on a save
// that had nothing to do with the password.
func TestASecretBeginningWithADollarSurvivesASave(t *testing.T) {
	t.Setenv("TEST_DOLLAR_SECRET_PW", "$w0rd!")
	text := "listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\npassword = \"$TEST_DOLLAR_SECRET_PW\"\n"
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Password: "$w0rd!", ConfigPath: path})

	// Any unrelated save at all: this is the one that used to lock.
	if _, err := s.writeAndApply(text + "\n[[camera]]\nname = \"gate\"\naddress = \"192.0.2.10\"\nusername = \"admin\"\npassword = \"\"\nstreams = [\"main\"]\n"); err != nil {
		t.Fatal(err)
	}
	if !s.authNow().Check("$w0rd!") {
		t.Fatal("saving an unrelated change locked the page out from its own password")
	}
}

// Learning that there is nothing to claim here after typing a password is
// learning it too late.
func TestTheClaimScreenSaysUpFrontWhenAConfigIsAlreadyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	rec := getFrom(t, s, "/claim")
	body := rec.Body.String()
	if !strings.Contains(body, path) || !strings.Contains(body, "nothing to claim") {
		t.Fatalf("the claim screen does not say a config is already there:\n%s", body)
	}
	if strings.Contains(body, `type="password"`) {
		t.Fatal("the claim screen still offers a form that POST will refuse")
	}
}

// A config caught half-written, or one that will not parse, must not be
// enough to drop the gate: that is how an install ends up with no claim
// screen and no password at the same time.
func TestABrokenConfigLeavesTheInstallUnclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if s.claimed() {
		t.Fatal("a config that will not parse counted as claimed")
	}
	if rec := getFrom(t, s, "/"); rec.Header().Get("Location") != "/claim" {
		t.Fatalf("/ went to %q, want /claim", rec.Header().Get("Location"))
	}

	// And the claim must not write over it: unclaimed does not mean there
	// is nothing there, and that file may be somebody's whole fleet.
	rec := postClaim(t, s, "correct-horse-battery")
	if rec.Code != http.StatusConflict {
		t.Fatalf("claiming over a broken config answered %d, want 409", rec.Code)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "listen = \"unterminated\n" {
		t.Fatalf("the broken config was overwritten: %q, %v", b, err)
	}
	if !strings.Contains(rec.Body.String(), path) {
		t.Fatalf("the refusal does not name the file to fix:\n%s", rec.Body.String())
	}
}

// An operator's deliberate allow_no_password install is claimed, and stays
// open, because that is what they asked for. Adoption never loosens
// anything either: it cannot turn AllowNoPassword back on.
func TestAnAllowNoPasswordConfigIsClaimedAndStaysOpen(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeHandConfig(t, "allow_no_password = true"),
	})
	if !s.claimed() {
		t.Fatal("a hand-written allow_no_password config did not count as claimed")
	}
	if rec := getFrom(t, s, "/claim"); rec.Code != http.StatusNotFound {
		t.Fatalf("/claim answered %d, want 404", rec.Code)
	}
	if !s.authNow().AllowNoPassword {
		t.Fatal("the live auth state stopped matching the operator's own config")
	}
}

// Adding a password through the Config page is the other way an install
// stops being open, and it has to take effect in the same breath: a
// "saved" banner from a page that still serves without a password is worse
// than no banner at all.
func TestAPasswordSavedFromTheConfigPageIsRequiredWithoutARestart(t *testing.T) {
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      writeHandConfig(t, "allow_no_password = true"),
	})
	if _, err := s.writeAndApply("listen = \"0.0.0.0:8560\"\n\n[control]\nlisten = \"0.0.0.0:8562\"\npassword = \"typed-in-the-editor\"\n"); err != nil {
		t.Fatal(err)
	}
	if !s.authNow().Check("typed-in-the-editor") {
		t.Fatal("a password saved from the config page was not adopted")
	}
	rec := getFrom(t, s, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("after saving a password, / answered %d to %q, want 303 to /login",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestAPasswordThatIsNotValidTextIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	// Not reachable from a browser; reachable from curl --data-binary.
	// %q would write it as an escape TOML reads back as something else, so
	// the password would work until the next restart and not after it.
	req := httptest.NewRequest("POST", "/claim",
		strings.NewReader("token="+url.QueryEscape(s.claimToken())+"&password=good\x92bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.168.1.10:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a password that is not valid UTF-8 was accepted")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a refused password was written anyway")
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
	if rec := postClaim(t, s, "correct-horse-battery"); rec.Code != http.StatusSeeOther {
		t.Errorf("claim returned %d", rec.Code)
	}
	close(stop)
	wg.Wait()
}
