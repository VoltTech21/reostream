package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unclaimedServer is a fresh install: no config file, the wide-open
// first-run auth state, and a token nobody has spent.
func unclaimedServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := newTestServer(t, Options{
		AllowNoPassword: true,
		ConfigPath:      filepath.Join(t.TempDir(), "config.toml"),
	})
	return s, s.claimToken()
}

func TestAClaimTokenIsBigEnoughAndUnambiguous(t *testing.T) {
	tok, err := newClaimToken()
	if err != nil {
		t.Fatal(err)
	}
	groups := strings.Split(tok, "-")
	if len(groups) != claimTokenChars/claimTokenGroup {
		t.Fatalf("token %q has %d groups, want %d", tok, len(groups), claimTokenChars/claimTokenGroup)
	}
	body := strings.ReplaceAll(tok, "-", "")
	if len(body) != claimTokenChars {
		t.Fatalf("token %q carries %d characters, want %d", tok, len(body), claimTokenChars)
	}
	// Every character has to come from the unambiguous alphabet, or an
	// operator copying it off a terminal is being asked to tell an I from
	// a 1. This is what makes canonicalClaimToken's refusal to guess safe.
	for _, r := range body {
		if !strings.ContainsRune(claimTokenAlphabet, r) {
			t.Fatalf("token %q contains %q, which is not in the alphabet", tok, r)
		}
	}
	for _, bad := range []rune{'0', 'O', '1', 'I', 'L'} {
		if strings.ContainsRune(claimTokenAlphabet, bad) {
			t.Errorf("the alphabet contains the ambiguous character %q", bad)
		}
	}
}

// Held in memory only. A restart while the install is still unclaimed must
// produce a different token, or "never written to disk" would be a
// distinction without a difference.
func TestARestartWhileUnclaimedPrintsANewToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	first := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	second := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if first.claimToken() == second.claimToken() {
		t.Fatal("two processes on the same unclaimed install share a token")
	}
	// And the first process's token is not accepted by the second, which is
	// the operational half of the same fact.
	if rec := postClaimFrom(t, second, "192.168.1.10:5000", first.claimToken(), "correct-horse"); rec.Code == http.StatusSeeOther {
		t.Fatal("a token from a previous process still claims this one")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a refused claim wrote a config")
	}
}

func TestAWrongClaimTokenIsRefused(t *testing.T) {
	for _, tok := range []string{
		"",
		"WRNG-TKEN-WRNG-TKEN",
		// A prefix of the real one, and the real one with something
		// appended: both must fail, and neither may be treated as a match
		// by the fixed-width compare.
		"prefix",
		"suffix",
	} {
		s, real := unclaimedServer(t)
		path := s.opts.ConfigPath
		got := tok
		switch tok {
		case "prefix":
			got = real[:len(real)-2]
		case "suffix":
			got = real + "9"
		}
		rec := postClaimFrom(t, s, "192.168.1.10:5000", got, "correct-horse")
		if rec.Code != http.StatusForbidden {
			t.Errorf("token %q answered %d, want 403", got, rec.Code)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("token %q was refused but a config was written anyway", got)
		}
		if s.claimed() {
			t.Errorf("token %q claimed the install", got)
		}
	}
}

// The token is the gate, not the address. A public source with the right
// token claims; that is the whole point of replacing the address rule,
// because the operator reaching a fresh install over a tailnet (100.64/10,
// which is not "private") was being refused on their own daemon.
func TestTheRightTokenClaimsFromAnyAddress(t *testing.T) {
	for _, addr := range []string{
		"100.101.102.103:5000", // a tailnet peer
		"8.8.8.8:5000",         // the public internet
		"127.0.0.1:5000",       // behind docker-proxy, which adds no headers
	} {
		s, tok := unclaimedServer(t)
		rec := postClaimFrom(t, s, addr, tok, "correct-horse")
		if rec.Code != http.StatusSeeOther {
			t.Errorf("a claim from %s with the right token answered %d, want 303; body: %s",
				addr, rec.Code, rec.Body.String())
		}
		if !s.authNow().Check("correct-horse") {
			t.Errorf("a claim from %s did not take effect", addr)
		}
	}
}

// A forwarding header is no longer a refusal: it was only ever evidence
// about a source address nothing looks at any more.
func TestAClaimThroughAProxyWithTheTokenSucceeds(t *testing.T) {
	s, tok := unclaimedServer(t)
	req := httptest.NewRequest("POST", "/claim",
		strings.NewReader("token="+tok+"&password=correct-horse"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a proxied claim with the right token answered %d, want 303", rec.Code)
	}
}

// Single use. Once an install is claimed the token is spent, and the route
// it was for does not exist any more.
func TestTheClaimTokenIsSingleUse(t *testing.T) {
	s, tok := unclaimedServer(t)
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, "correct-horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first claim answered %d, want 303", rec.Code)
	}
	if s.claimToken() != "" {
		t.Fatal("the token survived the claim it authorised")
	}
	if rec := postClaimFrom(t, s, "192.168.1.10:5000", tok, "attacker"); rec.Code != http.StatusNotFound {
		t.Fatalf("a second claim with the same token answered %d, want 404", rec.Code)
	}
	if !s.authNow().Check("correct-horse") {
		t.Fatal("the second claim changed the password")
	}
}

// A spent token must not match anything, least of all an empty submission:
// claimTokenMatches is asked about "" as the wanted value the moment a
// claim clears it.
func TestASpentTokenMatchesNothing(t *testing.T) {
	for _, got := range []string{"", "-", "   ", "ABCD-EFGH-JKMN-PQRS"} {
		if claimTokenMatches("", got) {
			t.Errorf("a spent token matched %q", got)
		}
	}
}

// People paste tokens lowercase, without the dashes, with spaces instead of
// dashes, and with whatever whitespace came along for the ride.
func TestAClaimTokenIsAcceptedHoweverItIsTyped(t *testing.T) {
	_, tok := unclaimedServer(t)
	bare := strings.ReplaceAll(tok, "-", "")
	for _, typed := range []string{
		tok,
		strings.ToLower(tok),
		bare,
		strings.ToLower(bare),
		"  " + tok + "\n",
		strings.ReplaceAll(tok, "-", " "),
	} {
		if !claimTokenMatches(tok, typed) {
			t.Errorf("the token typed as %q was not accepted", typed)
		}
	}
	// And one that really is different still is not.
	if claimTokenMatches(tok, strings.ReplaceAll(bare, bare[:1], "Z")) {
		t.Error("a changed token was accepted")
	}
}

// The compare must be constant-time: == stops at the first differing byte,
// which turns 79 bits into sixteen guesses of thirty-one. Timing cannot be
// asserted reliably in a unit test, so the call itself is asserted --
// crypto/subtle is used, and the source does not compare the token with ==.
func TestTheClaimTokenCompareUsesConstantTime(t *testing.T) {
	src, err := os.ReadFile("claimtoken.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, "subtle.ConstantTimeCompare") {
		t.Error("claimTokenMatches does not use subtle.ConstantTimeCompare")
	}
	if !strings.Contains(text, "subtle.ConstantTimeEq") {
		t.Error("the length check is not constant-time either")
	}
	// The body of claimTokenMatches must not compare the two canonical
	// strings directly.
	body := text[strings.Index(text, "func claimTokenMatches"):]
	if strings.Contains(body, "w == g") || strings.Contains(body, "want == got") {
		t.Error("claimTokenMatches compares the token with ==")
	}
}

// The token goes to the log and nowhere else. A page that printed it would
// hand the install to exactly the stranger the token exists to keep out.
func TestTheClaimTokenNeverReachesAResponse(t *testing.T) {
	s, tok := unclaimedServer(t)
	bare := strings.ReplaceAll(tok, "-", "")

	seen := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		body := rec.Body.String()
		if strings.Contains(body, tok) || strings.Contains(body, bare) {
			t.Errorf("%s put the token in the response body", what)
		}
		for k, vs := range rec.Header() {
			for _, v := range vs {
				if strings.Contains(v, tok) || strings.Contains(v, bare) {
					t.Errorf("%s put the token in the %s header", what, k)
				}
			}
		}
	}

	seen(getFrom(t, s, "/claim"), "the claim screen")
	seen(getFrom(t, s, "/"), "the claim gate's redirect")
	// The Logs page serves the same buffer the token was printed into, so
	// it had better be unreachable while the install is unclaimed.
	if rec := getFrom(t, s, "/logs/history"); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/claim" {
		t.Fatalf("an unclaimed install answered %d to /logs/history, want a redirect to /claim", rec.Code)
	}
	seen(postClaimFrom(t, s, "192.168.1.10:5000", "WRNG-TKEN-WRNG-TKEN", "correct-horse"), "a refused claim")
	seen(postClaimFrom(t, s, "192.168.1.10:5000", tok, "$nope"), "a refused password")
	seen(postClaimFrom(t, s, "192.168.1.10:5000", tok, "correct-horse"), "a successful claim")
}
