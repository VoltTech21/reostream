package control

import (
	"crypto/rand"
	"crypto/subtle"
	"strings"
	"unicode"
)

// Claiming an install is gated on a one-time token this process prints to
// its own log at startup, and nowhere else.
//
// What the token proves is "you can read this daemon's logs", and that is
// the property actually worth testing: reading the logs means controlling
// the deployment, which is what owning an install should mean. It is the
// same thing Jupyter, Portainer and Home Assistant do, and it works
// identically however the page is reached.
//
// It replaces an earlier rule that only answered a private source address.
// That rule failed in both directions, from one wrong premise -- that a
// source address tells you who may own an install:
//
//   - It refused legitimate operators. netip's IsPrivate is false for
//     100.64.0.0/10, which is where every Tailscale address lives, so
//     reaching a fresh install over a tailnet -- the normal way this fleet
//     is reached -- was refused on the operator's own daemon.
//   - It admitted strangers. docker-proxy is a plain TCP relay and adds no
//     HTTP headers, so a published container port makes every request
//     arrive from 127.0.0.1 or 172.17.0.1 and nothing can tell. The same
//     goes for Docker Desktop's gateway, a Kubernetes NodePort with
//     externalTrafficPolicy Cluster, nginx or HAProxy in stream mode,
//     socat and ssh -L. This repo ships a Dockerfile and a compose file,
//     so that is the product's own deployment shape, not an exotic one.
//
// No longer header list and no trusted-proxy knob fixes that, because the
// premise is what is wrong. Hence a token.
//
// The token lives in memory only and is never written to disk. A restart
// while the install is still unclaimed generates and prints a new one; the
// log says so. Persisting it would put a credential on disk that outlives
// the process for no gain -- the log line is regenerated at every startup
// anyway.
//
// It is printed straight to stderr and NOT through the log package, which
// tees into the in-memory buffer the Logs page serves. See printFirstRun in
// cmd/reostream: the reasoning for why a token in that buffer would
// probably still be safe exists, and it is thin enough that keeping the
// secret out of the buffer altogether is the better answer.

// claimTokenAlphabet has no visually ambiguous characters: no 0 or O, no 1,
// I or L. The token exists to be read off a terminal and typed into a
// browser, often from a phone photo of a `docker logs` window, and "was
// that a one or an ell" is the failure that makes a person give up. Every
// character here survives that trip, so nothing downstream has to guess at
// what an operator meant.
const claimTokenAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// claimTokenChars is how many characters the token carries, and
// claimTokenGroup how many go in each dash-separated group.
//
// The alphabet has 31 characters, so each one is log2(31) = 4.954 bits and
// 16 of them is 79.3 bits of entropy -- comfortably past the 60-bit floor.
// The entropy is the whole control here: nothing on this page is rate
// limited, and serveLogin records why nothing on it can be. Nothing slows
// a guesser down and nothing needs to -- at a million attempts a second,
// which no HTTP handler will serve, the expected search is still longer
// than the age of the universe by a wide margin. Three groups of four
// would have been 59.4 bits, just under that floor, which is why there are
// four groups and not three.
const (
	claimTokenChars = 16
	claimTokenGroup = 4
)

// newClaimToken returns a fresh token in its display form, dash-separated
// for transcription: XXXX-XXXX-XXXX-XXXX.
func newClaimToken() (string, error) {
	return newGroupedCode()
}

// newSuggestedPassword returns a password for the claim screen to offer,
// in the same readable dash-separated form and from the same generator as
// the token: 16 characters of crypto/rand from the 31-character alphabet,
// 79.3 bits. Grouped with dashes for transcription, so what is rendered and
// submitted is 19 runes; the 16 is the entropy, not the field length, and
// the dashes are part of the password rather than separators to strip. It is generated fresh on every render and is NEVER stored,
// logged or reused; see claimFormPage, which is the only thing that calls
// it, and claimPage.Suggested, which explains why rendering it into a
// response body is safe.
func newSuggestedPassword() (string, error) {
	return newGroupedCode()
}

// newGroupedCode is the generator both of the above are: claimTokenChars
// characters of crypto/rand from claimTokenAlphabet, grouped for reading.
//
// Rejection sampling, not a plain modulo: 256 is not a multiple of 31, so
// mapping every byte with % would make the first 8 characters of the
// alphabet slightly likelier than the rest. The bias is small, but the
// cost of avoiding it is a loop, and this runs once per process for the
// token and once per claim-screen render for the password.
func newGroupedCode() (string, error) {
	const limit = 256 - (256 % len(claimTokenAlphabet)) // 248
	out := make([]byte, 0, claimTokenChars)
	buf := make([]byte, claimTokenChars)
	for len(out) < claimTokenChars {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, claimTokenAlphabet[int(b)%len(claimTokenAlphabet)])
			if len(out) == claimTokenChars {
				break
			}
		}
	}
	return groupClaimToken(string(out)), nil
}

// groupClaimToken inserts the dashes a person reads the token by.
func groupClaimToken(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%claimTokenGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// canonicalClaimToken is what both sides of the comparison are reduced to
// before they are compared, so that a token typed the way people actually
// type one still matches: lowercase, with the dashes or without them, with
// spaces in place of the dashes, and with whatever leading or trailing
// whitespace a copy and paste picked up.
//
// Nothing is guessed at here beyond case and separators. The alphabet
// deliberately contains no character that can be confused for another, so
// there is no O-for-0 substitution to make and no reason to invent one.
func canonicalClaimToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		if r == '-' || r == '_' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// claimTokenMatches reports whether got is want, comparing in constant
// time.
//
// subtle.ConstantTimeCompare, never ==: == stops at the first differing
// byte, so an attacker who can time the answer learns the token one
// character at a time and 79 bits of entropy become 16 guesses of 31.
//
// ConstantTimeCompare short-circuits on a length mismatch, though -- it
// returns 0 immediately without looking at either argument -- so feeding it
// the canonical strings directly would leak the token's length through
// timing. Both sides are therefore copied into fixed-size arrays first, so
// the comparison always runs over exactly claimTokenChars bytes whatever
// was submitted. The length equality that copy would otherwise hide (a
// candidate that is the real token plus trailing characters would truncate
// to a match) is then re-checked with ConstantTimeEq and folded in with a
// bitwise and, so neither branch of the answer returns early.
func claimTokenMatches(want, got string) bool {
	w := canonicalClaimToken(want)
	g := canonicalClaimToken(got)
	// An empty token is not a token. A claimed install clears it, and
	// nothing may then match it -- least of all an empty submission.
	if w == "" {
		return false
	}
	var wb, gb [claimTokenChars]byte
	copy(wb[:], w)
	copy(gb[:], g)
	same := subtle.ConstantTimeCompare(wb[:], gb[:])
	sameLen := subtle.ConstantTimeEq(int32(len(w)), int32(len(g)))
	return same&sameLen == 1
}

// claimToken returns the token this process will accept for a claim, or ""
// once it has been spent. Read under authMu, the same lock the rest of the
// claim state lives behind, because a claim writes it from one request
// while others are reading it.
func (s *Server) claimToken() string {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.claimTok
}

// ClaimToken is the token an unclaimed install will accept, for the caller
// that has to print it: main logs it at startup and repeats it while the
// install stays unclaimed. It is the ONLY way the token leaves this
// process. It must not reach a response body, a URL, an error or a
// template -- anyone who could read it there could claim the install, and
// the whole point of the token is that only someone who can read the
// daemon's logs can.
func (s *Server) ClaimToken() string {
	return s.claimToken()
}
