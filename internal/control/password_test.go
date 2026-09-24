package control

import (
	"strings"
	"testing"
)

// The config's own bytes are edited, never a re-encoded TOML document.
// config.LoadRaw exists because re-encoding a loaded config once wrote
// every camera's real password into the file in place of the $VARIABLE
// references that were there, and a password change must not be the thing
// that reintroduces that.
func TestChangingThePasswordEditsOnlyThePasswordLine(t *testing.T) {
	raw := `listen = "0.0.0.0:8560"

[control]
listen = "0.0.0.0:8562"
password = "old-one-here-1234"

[[camera]]
name = "gate"
address = "192.0.2.50"
username = "admin"
password = "$GATE_PW"
streams = ["main"]
`
	out, err := setControlPassword(raw, "a-much-longer-one")
	if err != nil {
		t.Fatalf("setControlPassword: %v", err)
	}
	if !strings.Contains(out, `password = "a-much-longer-one"`) {
		t.Errorf("the new password is not in the file:\n%s", out)
	}
	if strings.Contains(out, "old-one-here-1234") {
		t.Errorf("the old password survived:\n%s", out)
	}
	// The camera's reference must come through untouched. This is the
	// whole reason for editing bytes instead of re-encoding.
	if !strings.Contains(out, `password = "$GATE_PW"`) {
		t.Errorf("the camera's $VARIABLE reference was rewritten:\n%s", out)
	}
	if !strings.Contains(out, `name = "gate"`) || !strings.Contains(out, `listen = "0.0.0.0:8560"`) {
		t.Errorf("something outside the password line changed:\n%s", out)
	}
}

// An install whose page password comes from the environment is refused,
// not rewritten: writing a literal would work and would also detach the
// file from the variable the daemon is started with, so the next deploy
// would set one password while the page wanted another.
func TestAPasswordReadFromTheEnvironmentIsDetected(t *testing.T) {
	raw := `[control]
listen = "0.0.0.0:8562"
password = "$REOSTREAM_CONTROL_PASSWORD"
`
	cur, ok := currentControlPassword(raw)
	if !ok {
		t.Fatal("no password found in a config that has one")
	}
	if cur != "$REOSTREAM_CONTROL_PASSWORD" {
		t.Fatalf("read %q, want the reference as written", cur)
	}
	if !strings.HasPrefix(cur, "$") {
		t.Error("the reference does not read as a reference")
	}
}

// A config with no [control] section, or none with a password in it, has
// to say so rather than silently writing nothing.
func TestChangingThePasswordRefusesAConfigWithNowhereToPutIt(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{"no control section", "listen = \"0.0.0.0:8560\"\n"},
		{"control with no password", "[control]\nlisten = \"0.0.0.0:8562\"\n"},
	} {
		if _, err := setControlPassword(c.raw, "a-much-longer-one"); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

// The password line inside [control] is the one that changes, not the
// first password line in the file: a camera block above the control
// section must not be the thing that gets rewritten.
func TestOnlyTheControlSectionsPasswordChanges(t *testing.T) {
	raw := `[[camera]]
name = "gate"
password = "camera-secret"

[control]
password = "page-secret-1234"
`
	out, err := setControlPassword(raw, "a-much-longer-one")
	if err != nil {
		t.Fatalf("setControlPassword: %v", err)
	}
	if !strings.Contains(out, `password = "camera-secret"`) {
		t.Errorf("the camera's password was rewritten:\n%s", out)
	}
	if strings.Contains(out, "page-secret-1234") {
		t.Errorf("the page password survived:\n%s", out)
	}
}
