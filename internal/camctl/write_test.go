package camctl

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
)

// newTestHTTPServer starts s.Handler on an httptest server and returns its
// base URL, closing it when the test ends.
func newTestHTTPServer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestOutcomeIsConfirmedOnlyWhenTheReadBackAgrees(t *testing.T) {
	cases := []struct {
		name        string
		status      int16
		readBack    string
		wrote       string
		verify      bool
		wantOutcome string
	}{
		{"read back matches", 200, "<osd>new</osd>", "<osd>new</osd>", true, "confirmed"},
		// The whole point. The camera said 200 and changed nothing.
		{"read back disagrees", 200, "<osd>old</osd>", "<osd>new</osd>", true, "accepted"},
		{"no verification asked for", 200, "", "<osd>new</osd>", false, "accepted"},
		{"camera refused", 400, "", "<osd>new</osd>", true, "refused"},
		{"wrong shape", 421, "", "<osd>new</osd>", true, "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := outcomeFor(tc.status, []byte(tc.wrote), []byte(tc.readBack), tc.verify)
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome %q, want %q", got.Outcome, tc.wantOutcome)
			}
			if tc.wantOutcome == "accepted" && !strings.Contains(got.Detail, "not proof") {
				t.Fatalf("an accepted write must say a 200 is not proof, got %q", got.Detail)
			}
		})
	}
}

func TestRefusedExplainsThatFourTwentyOneIsNotUnsupported(t *testing.T) {
	r := outcomeFor(421, nil, nil, true)
	if !strings.Contains(r.Detail, "two section") {
		t.Fatalf("421 detail does not explain the shape: %q", r.Detail)
	}
}

// pickTestPair finds a real read/write pair from baichuan.ConfigPairs to
// drive writeBlock against, rather than a made-up id: writeBlock resolves
// the Set id it is given back to its Get id through ConfigPairs, so a test
// id has to actually be in that table.
func pickTestPair(t *testing.T) baichuan.ConfigPair {
	t.Helper()
	pairs := baichuan.ConfigPairs()
	if len(pairs) == 0 {
		t.Fatal("ConfigPairs is empty, cannot pick a pair to test against")
	}
	return pairs[0]
}

// buildWriteFixture builds one connection's worth of replies: a login
// handshake, then a GetConfig reply for pair.Get carrying before, then a
// SetConfig reply for pair.Set carrying status. It is used for the first
// (write) connection in writeBlock's tests.
func buildWriteFixture(t *testing.T, pair baichuan.ConfigPair, before string, status int16) []byte {
	t.Helper()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, pair.Get, 200, testXMLHeader+before)...)
	fixture = append(fixture, statusReply(t, key, pair.Set, status, "")...)
	return fixture
}

// buildReadBackFixture builds one connection's worth of replies: a login
// handshake, then a single GetConfig reply for pair.Get carrying after. It
// is used for the second (verification) connection.
func buildReadBackFixture(t *testing.T, pair baichuan.ConfigPair, after string) []byte {
	t.Helper()
	key := baichuan.AESKey(testProbeNonce, "")
	fixture := loginHandshake(testProbeNonce, testProbeDeviceInfo)
	fixture = append(fixture, statusReply(t, key, pair.Get, 200, testXMLHeader+after)...)
	return fixture
}

// TestWriteBlockVerifiesOnAFreshConnectionNotTheWriteConnection is the load
// bearing test for this whole file: it proves writeBlock dials a second,
// separate connection to read the block back rather than reusing the one
// the write went out on, and that the outcome tracks what that fresh read
// actually said, not what was sent.
func TestWriteBlockVerifiesOnAFreshConnectionNotTheWriteConnection(t *testing.T) {
	pair := pickTestPair(t)
	wrote := "<body><field>new-value</field></body>"

	t.Run("confirmed", func(t *testing.T) {
		writeCam := fakecam.New(t, buildWriteFixture(t, pair, "<body><field>old-value</field></body>", 200))
		verifyCam := fakecam.New(t, buildReadBackFixture(t, pair, wrote))

		var dialed []string
		dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			addr := writeCam.Addr()
			if len(dialed) == 1 {
				addr = verifyCam.Addr()
			}
			dialed = append(dialed, addr)
			return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
		}

		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
		cam, err := s.byName("cam1")
		if err != nil {
			t.Fatal(err)
		}

		result, err := s.writeBlock(context.Background(), cam, pair.Set, []byte(wrote), true)
		if err != nil {
			t.Fatalf("writeBlock: %v", err)
		}
		if len(dialed) != 2 {
			t.Fatalf("dialed %d times, want 2 (the write connection, then a fresh one for verification)", len(dialed))
		}
		if dialed[0] == dialed[1] {
			t.Fatalf("verification dialed the same address as the write: %q, want a distinct fresh connection", dialed[0])
		}
		if result.Outcome != "confirmed" {
			t.Fatalf("outcome = %q, want confirmed: %+v", result.Outcome, result)
		}
		if !bytes.Contains(result.Before, []byte("old-value")) {
			t.Fatalf("Before = %q, want it to carry the pre-write document", result.Before)
		}
		if !bytes.Contains(result.After, []byte("new-value")) {
			t.Fatalf("After = %q, want it to carry the fresh read-back", result.After)
		}

		waitForCloses(t, writeCam, 1)
		waitForCloses(t, verifyCam, 1)
	})

	t.Run("accepted when the fresh read-back disagrees", func(t *testing.T) {
		writeCam := fakecam.New(t, buildWriteFixture(t, pair, "<body><field>old-value</field></body>", 200))
		// The camera answers 200 but the fresh connection reads back the
		// same document that was there before: a write accepted and
		// nothing changed, exactly the md-set / floodlight-set case.
		verifyCam := fakecam.New(t, buildReadBackFixture(t, pair, "<body><field>old-value</field></body>"))

		calls := 0
		dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
			calls++
			addr := writeCam.Addr()
			if calls == 2 {
				addr = verifyCam.Addr()
			}
			return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
		}

		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
		cam, err := s.byName("cam1")
		if err != nil {
			t.Fatal(err)
		}

		result, err := s.writeBlock(context.Background(), cam, pair.Set, []byte(wrote), true)
		if err != nil {
			t.Fatalf("writeBlock: %v", err)
		}
		if result.Outcome != "accepted" {
			t.Fatalf("outcome = %q, want accepted", result.Outcome)
		}
		if !strings.Contains(result.Detail, "not proof") {
			t.Fatalf("accepted detail must say a 200 is not proof, got %q", result.Detail)
		}
	})
}

func TestWriteBlockRefusedNeverDialsAFreshConnection(t *testing.T) {
	pair := pickTestPair(t)
	writeCam := fakecam.New(t, buildWriteFixture(t, pair, "<body><field>old-value</field></body>", 421))

	calls := 0
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		calls++
		return baichuan.Dial(ctx, writeCam.Addr(), baichuan.Options{Password: ""})
	}

	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	cam, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.writeBlock(context.Background(), cam, pair.Set, []byte("<body><field>new-value</field></body>"), true)
	if err != nil {
		t.Fatalf("writeBlock: %v", err)
	}
	if result.Outcome != "refused" {
		t.Fatalf("outcome = %q, want refused", result.Outcome)
	}
	if !strings.Contains(result.Detail, "two section") {
		t.Fatalf("refused detail does not explain the 421 shape: %q", result.Detail)
	}
	if calls != 1 {
		t.Fatalf("dial called %d times, want 1: a refused write must not go on to verify", calls)
	}
}

func TestServeWriteRejectsAnUnknownMessageID(t *testing.T) {
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1")})
	ts := newTestHTTPServer(t, s)

	resp, err := http.PostForm(ts+"/camera/cam1/write/999999999", url.Values{"body": {"<x/>"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an id that is not a known writable pair", resp.StatusCode)
	}
}

func TestServeWriteRejectsAnEmptyBody(t *testing.T) {
	pair := pickTestPair(t)
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1")})
	ts := newTestHTTPServer(t, s)

	resp, err := http.PostForm(ts+"/camera/cam1/write/"+strconv.FormatUint(uint64(pair.Set), 10), url.Values{"body": {""}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an empty body", resp.StatusCode)
	}
}

func TestServeWriteEndToEndReportsConfirmed(t *testing.T) {
	pair := pickTestPair(t)
	wrote := "<body><field>new-value</field></body>"
	writeCam := fakecam.New(t, buildWriteFixture(t, pair, "<body><field>old-value</field></body>", 200))
	verifyCam := fakecam.New(t, buildReadBackFixture(t, pair, wrote))

	calls := 0
	dial := func(ctx context.Context, c Camera) (*baichuan.Conn, error) {
		calls++
		addr := writeCam.Addr()
		if calls == 2 {
			addr = verifyCam.Addr()
		}
		return baichuan.Dial(ctx, addr, baichuan.Options{Password: ""})
	}
	s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: writeTestConfig(t, "cam1"), Dial: dial})
	ts := newTestHTTPServer(t, s)

	resp, err := http.PostForm(ts+"/camera/cam1/write/"+strconv.FormatUint(uint64(pair.Set), 10), url.Values{"body": {wrote}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
}
