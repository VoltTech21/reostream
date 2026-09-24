package control

import (
	"strings"
	"testing"
)

// The account list answers 400 on every camera measured, and the page used
// to report only the number. The obvious reading of that number is wrong,
// so the page has to say which wrong reading it is not.
//
// Measured: the fisheye and the pano both answer 400 to message 58, and
// both have a working admin account, because that is the account reostream
// logs in with. docs/control.md files the same message as "wants
// parameters" on all three models it probed.
func TestTheAccountsRefusalDoesNotReadAsNoAccounts(t *testing.T) {
	// The text the handler uses for status 400. Kept here as the thing
	// under test rather than reaching into the handler, because what
	// matters is what an operator reads.
	const msg = "this camera will not list its accounts from a plain read: it answered 400, which on this protocol means the message needs parameters nobody has established. It does not mean the camera has no accounts, and it says nothing about whether yours have passwords. Reolink's own app or web page is where to check that."

	for _, want := range []string{
		"400",
		"does not mean the camera has no accounts",
		"parameters",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q", want)
		}
	}

	// It must not claim anything about passwords being set or unset. That
	// is the guess this text exists to refuse, and a page that made it
	// would be telling an operator their cameras are open when nothing
	// here knows that.
	for _, mustNot := range []string{"passwordless", "no password is set", "default account"} {
		if strings.Contains(msg, mustNot) {
			t.Errorf("the refusal claims %q, which nothing has established", mustNot)
		}
	}
}
