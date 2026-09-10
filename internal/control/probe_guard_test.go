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

func writeTestConfig(t *testing.T, body string) string {
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

// The whole point of the guard: a camera the daemon is actively streaming
// must never be dialled again by a probe, because this protocol permits
// exactly one connection per stream.
func TestAlreadyStreamingBlocksAConfiguredLiveCamera(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)
	s := New(Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: true, Streaming: true},
		},
	})

	msg, blocked := s.alreadyStreaming("192.0.2.50")
	if !blocked {
		t.Fatal("want blocked = true for a camera the daemon reports connected")
	}
	for _, want := range []string{"front", "main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// The default port rule (":9000") must not let a typed address slip past
// the match against a config entry written without a port, or the guard is
// trivially bypassed by typing the address slightly differently.
func TestAlreadyStreamingMatchesOnNormalizedAddress(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)
	s := New(Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: true},
		},
	})

	if _, blocked := s.alreadyStreaming("192.0.2.50:9000"); !blocked {
		t.Error("want the guard to match an explicit :9000 against a config entry with no port")
	}
}

// A camera that is configured but currently down (no live connection to
// contend with) must not block a probe: the guard exists to prevent
// overlapping connections, not to forbid re-probing an offline camera.
func TestAlreadyStreamingAllowsAConfiguredButDownCamera(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)
	s := New(Options{
		ConfigPath: path,
		Status: stubStatus{
			"front/main": server.StreamStatus{Connected: false},
			"front/sub":  server.StreamStatus{Connected: false},
		},
	})

	if _, blocked := s.alreadyStreaming("192.0.2.50"); blocked {
		t.Error("want blocked = false for a camera that is configured but not connected")
	}
}

// An address matching nothing in config must never block: most probes are
// for a camera not yet added, which is the ordinary case this page exists
// for.
func TestAlreadyStreamingAllowsAnUnconfiguredAddress(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)
	s := New(Options{
		ConfigPath: path,
		Status:     stubStatus{"front/main": server.StreamStatus{Connected: true}},
	})

	if _, blocked := s.alreadyStreaming("198.51.100.9"); blocked {
		t.Error("want blocked = false for an address with no matching camera")
	}
}

// Without both a config path and a StatusSource, the question genuinely
// cannot be answered here, and the guard must say so by never blocking
// rather than silently pretending nothing is live.
func TestAlreadyStreamingCannotAnswerWithoutConfigOrStatus(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)

	noStatus := New(Options{ConfigPath: path})
	if _, blocked := noStatus.alreadyStreaming("192.0.2.50"); blocked {
		t.Error("want blocked = false with no StatusSource wired")
	}

	noConfig := New(Options{Status: stubStatus{"front/main": server.StreamStatus{Connected: true}}})
	if _, blocked := noConfig.alreadyStreaming("192.0.2.50"); blocked {
		t.Error("want blocked = false with no ConfigPath wired")
	}
}

// probeGuarded must return the guard's message and never reach probeCamera
// (which would dial) when the guard blocks. Proven here by pointing at a
// closed local port: if the guard failed to block, this would attempt a
// real dial and fail differently (a dial error, not the guard's message).
func TestProbeGuardedNeverDialsWhenBlocked(t *testing.T) {
	path := writeTestConfig(t, guardTestConfig)
	s := New(Options{
		ConfigPath: path,
		Status:     stubStatus{"front/main": server.StreamStatus{Connected: true}},
	})

	rep := s.probeGuarded(context.Background(), "192.0.2.50", "admin", "")
	if rep.Model != "" || len(rep.Streams) != 0 {
		t.Fatalf("got a report with a model or streams, want only a blocked message: %+v", rep)
	}
	if !strings.Contains(rep.Err, "front") {
		t.Errorf("Err = %q, does not name the camera already streaming", rep.Err)
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
