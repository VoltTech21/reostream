package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/server"
)

// stubStatus is a StatusSource over a fixed map, for tests that need to
// claim a stream is live without a real supervisor.
type stubStatus map[string]server.StreamStatus

func (s stubStatus) StreamStats() map[string]server.StreamStatus { return s }

func writeRawTestConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "reostream.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const guardTestConfig = `
[[camera]]
name = "front"
address = "192.0.2.50"
username = "admin"
password = ""
streams = ["main", "sub"]
`

// The whole point of the guard: an address already in the daemon's config
// must never be dialled again by a probe, because this protocol permits
// exactly one connection per stream and a second one risks contending with,
// or triggering the same lockout as, whatever the daemon is already doing
// with that camera.
func TestAlreadyStreamingBlocksAConfiguredLiveCamera(t *testing.T) {
	path := writeRawTestConfig(t, guardTestConfig)
	s := newTestServer(t, Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: true, Streaming: true},
		},
	})

	msg, blocked := s.alreadyStreaming("192.0.2.50")
	if !blocked {
		t.Fatal("want blocked = true for a camera already in config")
	}
	for _, want := range []string{"front", "main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// This is the case the guard exists for most: a camera sitting
// reconnecting or down is not safer to probe than one streaming cleanly.
// It is very often *more* dangerous, since a held session the camera has
// not released is one of the more common reasons a stream never comes up,
// and the supervisor is mid-backoff, about to dial again at an
// unpredictable moment a probe would race rather than avoid. Blocking is a
// config-membership decision now, specifically so this case is refused too.
func TestAlreadyStreamingBlocksAConfiguredButDisconnectedCamera(t *testing.T) {
	path := writeRawTestConfig(t, guardTestConfig)
	s := newTestServer(t, Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: false, Restarts: 4},
			"front/sub":  server.StreamStatus{Connected: false, Restarts: 4},
		},
	})

	msg, blocked := s.alreadyStreaming("192.0.2.50")
	if !blocked {
		t.Fatal("want blocked = true for a configured camera even though no stream is currently connected")
	}
	if !strings.Contains(msg, "front") {
		t.Errorf("message %q does not name the camera", msg)
	}
}

// The default port rule (":9000") must not let a typed address slip past
// the match against a config entry written without a port, or the guard is
// trivially bypassed by typing the address slightly differently.
func TestAlreadyStreamingMatchesOnNormalizedAddress(t *testing.T) {
	path := writeRawTestConfig(t, guardTestConfig)
	s := newTestServer(t, Options{ConfigPath: path})

	if _, blocked := s.alreadyStreaming("192.0.2.50:9000"); !blocked {
		t.Error("want the guard to match an explicit :9000 against a config entry with no port")
	}
}

// An address matching nothing in config must never block: most probes are
// for a camera not yet added, which is the ordinary case this page exists
// for.
func TestAlreadyStreamingAllowsAnUnconfiguredAddress(t *testing.T) {
	path := writeRawTestConfig(t, guardTestConfig)
	s := newTestServer(t, Options{
		ConfigPath: path,
		Status:     stubStatus{"front/main": server.StreamStatus{Connected: true}},
	})

	if _, blocked := s.alreadyStreaming("198.51.100.9"); blocked {
		t.Error("want blocked = false for an address with no matching camera")
	}
}

// Without a config path, the question genuinely cannot be answered here,
// and the guard must say so by never blocking rather than refusing every
// probe just because it cannot check. A StatusSource is no longer required
// to decide whether to block at all -- it only affects what the blocked
// message says -- so a missing one alone must not block anything either.
func TestAlreadyStreamingCannotAnswerWithoutConfig(t *testing.T) {
	noConfig := newTestServer(t, Options{})
	if _, blocked := noConfig.alreadyStreaming("192.0.2.50"); blocked {
		t.Error("want blocked = false with no ConfigPath wired")
	}

	path := writeRawTestConfig(t, guardTestConfig)
	noStatus := newTestServer(t, Options{ConfigPath: path})
	if _, blocked := noStatus.alreadyStreaming("192.0.2.50"); !blocked {
		t.Error("want blocked = true for a configured camera even with no StatusSource wired; " +
			"blocking is a config-membership decision, Status only affects the message")
	}
}

// probeGuarded must return the guard's message and never reach probeCamera
// (which would dial) when the guard blocks. Proven here by pointing at a
// closed local port: if the guard failed to block, this would attempt a
// real dial and fail differently (a dial error, not the guard's message).
// The camera's streams are deliberately reported disconnected, since that
// is now the case that most needs the guard to hold.
func TestProbeGuardedNeverDialsWhenBlocked(t *testing.T) {
	path := writeRawTestConfig(t, guardTestConfig)
	s := newTestServer(t, Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: false},
			"front/sub":  server.StreamStatus{Connected: false},
		},
	})

	rep := s.probeGuarded(context.Background(), "192.0.2.50", "admin", "")
	if rep.Model != "" || len(rep.Streams) != 0 {
		t.Fatalf("got a report with a model or streams, want only a blocked message: %+v", rep)
	}
	if !strings.Contains(rep.Err, "front") {
		t.Errorf("Err = %q, does not name the camera already configured", rep.Err)
	}
}

// streamReport is the pure mapping GetStreamInfo's outcome goes through.
// This is where the tri-state contract actually lives, and it needs no
// network at all to test.
func TestStreamReportMarksAFailureUndeterminedNeverAbsent(t *testing.T) {
	got := streamReport("main", baichuan.StreamInfo{}, context.DeadlineExceeded)
	if got.Absent {
		t.Error("a failure must never be reported Absent: this path has no positive absence signal")
	}
	if !got.Undetermined {
		t.Error("want Undetermined = true")
	}
	if got.Reason == "" {
		t.Error("want a Reason explaining what went wrong")
	}
	if got.Codec != "" || got.Width != 0 || got.Height != 0 {
		t.Errorf("an undetermined report should carry no stream data, got %+v", got)
	}
}

func TestStreamReportMarksSuccessPresent(t *testing.T) {
	got := streamReport("main", baichuan.StreamInfo{Codec: "H265", Width: 3840, Height: 2160}, nil)
	if got.Absent || got.Undetermined {
		t.Errorf("a successful read must be neither Absent nor Undetermined, got %+v", got)
	}
	if got.Codec != "H265" || got.Width != 3840 || got.Height != 2160 {
		t.Errorf("stream data not carried through: %+v", got)
	}
}

// extern is the one stream with a genuine wire-given absence signal
// (Support.noExternStream), so it is the only one this code is allowed to
// mark Absent, and only from that flag, never from a failed read.
func TestExternAbsentReportIsMarkedAbsentWithAReason(t *testing.T) {
	got := externAbsentReport()
	if !got.Absent {
		t.Error("want Absent = true")
	}
	if got.Undetermined {
		t.Error("extern's absence is a known fact here, not an undetermined one")
	}
	if got.Reason == "" {
		t.Error("want a Reason")
	}
}
