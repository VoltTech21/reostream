package webui

import (
	"fmt"
	"testing"
)

// Outputs of the hand-written PBKDF2 this replaced, so the switch to
// crypto/pbkdf2 is pinned to change nothing a login sees.
func TestStretchMatchesStandardPBKDF2(t *testing.T) {
	for _, c := range []struct {
		pw   string
		iter int
		want string
	}{
		{"", 1, "f3f323749a46814d26285073ec8681c31ef4f9c3a45a05885d5360993b55cb72"},
		{"hunter2", 1000, "08b47dcb8eefda83e08a53a738b5946b5d67f9ccecf4b92d5c7a34c099441a84"},
		{"pässwörd", 16, "a49a2819f265cdc514f2d01114e62a4f08e99538611cc3a7ddd915d8416b2657"},
		{"correct horse battery staple", 100000, "f1b92d2641901c00593f9247bb30f87fddee6da41589ffdc77880bc758a5cd2a"},
	} {
		got, err := stretch(c.pw, c.iter)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%x", got) != c.want {
			t.Errorf("%q x%d: %x, want %s", c.pw, c.iter, got, c.want)
		}
	}
}
