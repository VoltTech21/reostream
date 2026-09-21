package control

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

// lineNumber matches the shape of a parser's complaint -- "line 7" -- as
// opposed to the word "line" appearing in ordinary prose.
var lineNumber = regexp.MustCompile(`line \d`)

// TestABadCharacterIsRefusedInTheOperatorsTerms covers the message, not the
// safety: a control character in a submitted field was already refused,
// because saveConfig validates through the real loader before writing
// anything. What the operator saw was a TOML parse error naming a line
// number in a file they never opened.
func TestABadCharacterIsRefusedInTheOperatorsTerms(t *testing.T) {
	cases := []struct {
		what  string
		form  url.Values
		wants string
	}{
		{
			what: "a newline in the name",
			form: url.Values{"name": {"lounge\nmore"}, "address": {"192.0.2.10"},
				"username": {"admin"}, "password": {""}, "streams": {"main"}},
			wants: "line break",
		},
		{
			what: "a NUL in the password",
			form: url.Values{"name": {"lounge"}, "address": {"192.0.2.10"},
				"username": {"admin"}, "password": {"a\x00b"}, "streams": {"main"}},
			wants: "control characters",
		},
		{
			what: "invalid text in the username",
			form: url.Values{"name": {"lounge"}, "address": {"192.0.2.10"},
				"username": {string([]rune{0xfffd})}, "password": {""}, "streams": {"main"}},
			wants: "not valid text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			cfg := &config.Config{Listen: "0.0.0.0:8560"}
			err := applyCameraForm(cfg, tc.form)
			if err == nil {
				t.Fatal("accepted a value that cannot survive the config file")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("message was %q, want it to mention %q", err, tc.wants)
			}
			// The message must name the field, not a line number in a file
			// the operator never opened. Matching a bare "line " was too
			// crude: it flagged the correct message "cannot contain a line
			// break". A line NUMBER is the tell.
			if strings.Contains(err.Error(), "toml") || lineNumber.MatchString(err.Error()) {
				t.Errorf("message still speaks in the config parser's terms: %q", err)
			}
		})
	}
}

// TestAnOrdinaryNameIsStillAccepted keeps the check narrow. Camera names on
// the fleet this was written against are things like "Sales Floor", and a
// validator that refused a space, an apostrophe or an accent would be worse
// than the problem it solves.
func TestAnOrdinaryNameIsStillAccepted(t *testing.T) {
	for _, name := range []string{"lounge", "Sales Floor", "Back Door 2", "Café", "shop-front_1", "n°3"} {
		cfg := &config.Config{Listen: "0.0.0.0:8560"}
		form := url.Values{"name": {name}, "address": {"192.0.2.10"},
			"username": {"admin"}, "password": {"p@ss w0rd!"}, "streams": {"main"}}
		if err := applyCameraForm(cfg, form); err != nil {
			t.Errorf("name %q was refused: %v", name, err)
		}
	}
}
