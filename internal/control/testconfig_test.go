package control

import (
	"os"
	"strings"
	"testing"
)

// TestTheTestConfigIsClaimed pins the other half of the "test that cannot
// fail" problem found in this package.
//
// Several tests built a config with no [control] section. On an unclaimed
// install every route answers 303 to /claim, so those tests never reached
// the handler they were written for, and because they asserted only that
// nothing had been destroyed, they passed anyway.
//
// writeTestConfig now writes allow_no_password, which makes the install
// claimed and the handlers reachable. This asserts that, so the helper
// cannot quietly lose it: hundreds of assertions in this package depend on
// a request arriving somewhere other than the claim screen, and none of
// them would say so if it stopped.
func TestTheTestConfigIsClaimed(t *testing.T) {
	path := writeTestConfig(t, "one")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[control]") {
		t.Fatal("writeTestConfig wrote no [control] section: every request in this package " +
			"would be answered by the claim screen instead of its handler")
	}

	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if !s.claimed() {
		t.Fatal("a server built on writeTestConfig's output is not claimed, so claimGate " +
			"intercepts every route before any handler sees it")
	}
}

// TestAnUnclaimedServerInterceptsEverything is the same property stated
// from the other side, so the test above is demonstrably testing something:
// with a config that has no [control] section, a request really is taken by
// the claim gate rather than the handler.
func TestAnUnclaimedServerInterceptsEverything(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"
	if err := os.WriteFile(path, []byte("listen = \"0.0.0.0:8560\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
	if s.claimed() {
		t.Fatal("a config with no [control] section counted as claimed")
	}
}
