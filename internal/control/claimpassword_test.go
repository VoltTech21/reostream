package control

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// suggestedFrom pulls the generated password out of the claim screen's
// HTML: the value attribute of the password input.
func suggestedFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="password" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("the claim screen has no generated password in it:\n%s", body)
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("the password input is malformed:\n%s", body)
	}
	return rest[:j]
}

// The claim screen offers a password, and a different one every time.
//
// Rendering a secret-shaped string in a response body is the one thing on
// this page that looks like a leak and is not, so the property that makes
// it safe is asserted here rather than only claimed in a comment: two GETs
// produce two unrelated values, which means an attacker who fetches this page a
// thousand times collects a thousand strings with no relationship to the
// one the operator eventually submits.
func TestTheClaimScreenGeneratesAFreshPasswordEveryTime(t *testing.T) {
	s, _ := unclaimedServer(t)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		rec := getFrom(t, s, "/claim")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /claim answered %d, want 200", rec.Code)
		}
		pw := suggestedFrom(t, rec.Body.String())
		if seen[pw] {
			t.Fatalf("GET %d returned a password that had already been offered: %q", i+1, pw)
		}
		seen[pw] = true

		// Long enough to satisfy the rule it is offered against, and built
		// from the unambiguous alphabet, so an operator reading it off the
		// screen cannot mistype it into something they cannot reproduce.
		if err := validClaimPassword(pw); err != nil {
			t.Fatalf("the generated password %q would be refused: %v", pw, err)
		}
		for _, r := range strings.ReplaceAll(pw, "-", "") {
			if !strings.ContainsRune(claimTokenAlphabet, r) {
				t.Fatalf("the generated password %q contains %q, which is not in the alphabet", pw, r)
			}
		}
	}
}

// The generated password is a suggestion and nothing more: it is not
// stored, so it is not something the server can be made to compare against
// or hand back. The only evidence of that available from outside is that
// it never appears anywhere except the body that offered it -- not in the
// log, not in the log buffer the Logs page serves, and not on disk.
func TestTheGeneratedPasswordIsNeverLoggedOrStored(t *testing.T) {
	// The same wiring main uses: the standard logger tees into the buffer
	// the Logs page serves, so one assertion covers both `docker logs` and
	// the page any signed-in operator can read.
	buf := NewLogBuffer(2000)
	var stderr bytes.Buffer
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(&stderr, buf))
	t.Cleanup(func() { log.SetOutput(prev) })

	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      filepath.Join(t.TempDir(), "config.toml"),
		Logs:            buf,
	})
	tok := s.claimToken()

	offered := suggestedFrom(t, getFrom(t, s, "/claim").Body.String())

	// A second GET, then a claim with a password of the operator's own:
	// the offered one was never used and must not survive anywhere.
	getFrom(t, s, "/claim")
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, "correct-horse-battery-staple"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the claim answered %d, want 303", rec.Code)
	}

	if strings.Contains(stderr.String(), offered) {
		t.Fatalf("the generated password reached the log:\n%s", stderr.String())
	}
	if strings.Contains(strings.Join(buf.Lines(), "\n"), offered) {
		t.Fatal("the generated password reached the log buffer the Logs page serves")
	}
	b, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), offered) {
		t.Fatalf("the generated password was written to the config file:\n%s", b)
	}
	// And the password that WAS submitted is the one in force.
	if !s.authNow().Check("correct-horse-battery-staple") {
		t.Fatal("the submitted password is not the one this process requires")
	}
}

// An operator who accepts the offer must be able to submit it unchanged:
// the value in the box has to pass the rule it is offered against, and
// claim the install.
func TestTheGeneratedPasswordCanBeSubmittedUnchanged(t *testing.T) {
	s, tok := unclaimedServer(t)

	offered := suggestedFrom(t, getFrom(t, s, "/claim").Body.String())
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, offered); rec.Code != http.StatusSeeOther {
		t.Fatalf("submitting the generated password answered %d, want 303", rec.Code)
	}
	if !s.claimed() {
		t.Fatal("the install is not claimed")
	}
	if !s.authNow().Check(offered) {
		t.Fatal("the generated password is not the one this process requires")
	}
	// And it survives the round trip to disk, which is what a restart would
	// read back.
	b, err := os.ReadFile(s.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), offered) {
		t.Fatalf("the claimed password is not in the config file:\n%s", b)
	}
}

// A generated password is offered on every render that draws the form,
// including the ones that draw it after a refusal -- otherwise an operator
// who mistyped the token would be sent back to an empty box and the offer
// would be a one-shot thing they had already lost.
func TestARefusedClaimIsOfferedAFreshPassword(t *testing.T) {
	s, tok := unclaimedServer(t)

	first := suggestedFrom(t, getFrom(t, s, "/claim").Body.String())

	wrongToken := postClaimFrom(t, s, "192.168.1.10:5000", "WRNG-TKEN-WRNG-TKEN", "correct-horse-battery-staple")
	if wrongToken.Code != http.StatusForbidden {
		t.Fatalf("a wrong token answered %d, want 403", wrongToken.Code)
	}
	afterToken := suggestedFrom(t, wrongToken.Body.String())

	shortPw := postClaimFrom(t, s, "192.168.1.10:5000", tok, "hunter2")
	if shortPw.Code != http.StatusBadRequest {
		t.Fatalf("a short password answered %d, want 400", shortPw.Code)
	}
	afterShort := suggestedFrom(t, shortPw.Body.String())

	if first == afterToken || first == afterShort || afterToken == afterShort {
		t.Fatalf("a refusal re-showed a password instead of generating one: %q %q %q", first, afterToken, afterShort)
	}
	// Still a suggestion: none of them claimed anything.
	if s.claimed() {
		t.Fatal("a refused claim claimed the install")
	}
}

// The offer does not outlive the claim. Once the install has an owner the
// route is gone, so there is no page left that hands out generated
// passwords, and no password field for an attacker to read.
func TestNoPasswordIsOfferedOnceClaimed(t *testing.T) {
	s, tok := unclaimedServer(t)
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, "correct-horse-battery-staple"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the claim answered %d, want 303", rec.Code)
	}
	rec := getFrom(t, s, "/claim")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /claim answered %d on a claimed install, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("the 404 still drew a password field:\n%s", rec.Body.String())
	}
}

// A page that cannot accept a password must not offer one: the config-file
// refusal draws no form, so there is nothing for a suggestion to go in.
func TestNoPasswordIsOfferedWhenThereIsNothingToClaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// A config file that exists and yields no [control] password: unclaimed,
	// but nothing here can be claimed either.
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n[control]\nlisten = \"0.0.0.0:8562\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})

	rec := getFrom(t, s, "/claim")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /claim answered %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("a page with nothing to claim drew a password field:\n%s", rec.Body.String())
	}
}

// postLogin submits a password from a chosen source.
func postLogin(t *testing.T, s *Server, remoteAddr, pw string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/login",
		strings.NewReader(url.Values{"password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The login has no rate limit, and this is the test that says so out loud
// so that a reader who finds no throttle knows it was removed rather than
// forgotten.
//
// The property is the one every throttle design failed: the correct
// password works, immediately, under any request pattern from any source.
// Four rounds of source-keyed limiting produced a lockout that refused the
// operator, a delay that refused them at 32 req/s, and two designs an
// attacker could open at 8,000 checks a second anyway -- because behind
// docker-proxy the attacker and the operator arrive from one address. See
// serveLogin.
func TestTheLoginNeverDelaysOrRefusesTheRightPassword(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "one")})
	handler := s.Handler()

	// A flood of wrong guesses from the same source the operator will use,
	// concurrent, sustained -- exactly the pattern that broke every earlier
	// round.
	stop := make(chan struct{})
	var wrong atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest("POST", "/login",
					strings.NewReader(url.Values{"password": {"wrong"}}.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.RemoteAddr = "172.17.0.1:5000"
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("a wrong password answered %d, want 401", rec.Code)
					return
				}
				wrong.Add(1)
			}
		}()
	}
	// Let the flood get going, so the operator's attempt lands in the
	// middle of it rather than ahead of it. Bounded, because a worker
	// that has bailed out with its own t.Errorf would otherwise leave this
	// spinning until the whole suite's timeout, and a test that hangs
	// reports nothing useful.
	deadline := time.Now().Add(30 * time.Second)
	for wrong.Load() < 200 {
		if time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("only %d wrong guesses were answered in 30s; the flood never got going", wrong.Load())
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	rec := postLogin(t, s, "172.17.0.1:5000", "hunter2")
	took := time.Since(start)
	close(stop)
	wg.Wait()

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d during a flood from the same source, want 303", rec.Code)
	}
	// Bounded by the HTTP server and the password comparison, nothing else.
	// Generous, because this is a timing assertion on a shared machine; a
	// queue deep enough to meter an attacker would be seconds or hours, not
	// milliseconds, so the order is what is being asserted.
	if took > time.Second {
		t.Fatalf("the right password took %v to be checked; something is metering the login", took)
	}
	// Repeated failures cost the next attempt nothing either: no lockout,
	// no escalating delay, no window to wait out.
	for i := 0; i < 50; i++ {
		if rec := postLogin(t, s, "172.17.0.1:5000", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d answered %d, want 401", i+1, rec.Code)
		}
	}
	start = time.Now()
	if rec := postLogin(t, s, "172.17.0.1:5000", "hunter2"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d after 50 failures, want 303", rec.Code)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the right password took %v after 50 failures; something is metering the login", took)
	}
}
