package stream

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
)

// TestRunCarriesAudioFromTheCameraToTheHub proves audio actually reaches a
// subscriber through the real Run loop, not just that ts.Muxer can carry
// it. Task 4 shipped a muxer able to mux audio attached to a stream.Run
// that never asked it to, which is the same failure the muxer was built to
// fix, wearing different clothes: production had declared an audio role
// for days while the pipe silently dropped every audio frame. This test
// runs against a fake camera replaying a real capture known to contain 56
// AAC frames (internal/baichuan's own TestDepacketiserOnH265Capture
// reports audio=56), collects everything Run publishes, and hands it to
// ffprobe to prove the audio track is not just present in the PMT but
// actually decodable.
func TestRunCarriesAudioFromTheCameraToTheHub(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(256)

	ch, cancelSub := h.Subscribe()
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{Name: "test", Address: cam.Addr(), Stream: "main"}
	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, cfg, h) }()

	var out bytes.Buffer
	if hdr := waitForHeader(t, h); hdr != nil {
		out.Write(hdr)
	}

	// fakecam writes the whole fixture and then just holds the connection
	// open, the way a real camera does once it has nothing left to say and
	// has not been told to stop (see fakecam.Camera.serve): there is no EOF
	// or channel close marking "the fixture is done" to wait on. An idle
	// gap on ch is the signal instead: once frames stop arriving for a
	// while, the fixture has been fully depacketised and muxed, so cancel
	// the context to end Run and stop the fake camera's connection.
	const idleGap = 500 * time.Millisecond
	idle := time.NewTimer(idleGap)
	defer idle.Stop()
	overall := time.After(10 * time.Second)
collect:
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				break collect
			}
			out.Write(chunk)
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(idleGap)
		case <-idle.C:
			break collect
		case <-overall:
			t.Fatal("timed out waiting for the fixture to finish")
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	tmp, err := os.CreateTemp(t.TempDir(), "reostream-*.ts")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := tmp.Write(out.Bytes()); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	cmd := exec.Command("ffprobe",
		"-v", "warning",
		"-count_frames",
		"-show_entries", "stream=codec_type,codec_name,nb_read_frames",
		"-of", "csv=p=0",
		tmp.Name(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffprobe failed: %v\nstderr: %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("ffprobe wrote to stderr: %s", stderr.String())
	}
	t.Logf("ffprobe output:\n%s", stdout.String())

	var sawVideo, sawAudio bool
	var audioFrames int
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		fields := strings.Split(l, ",")
		if len(fields) != 3 {
			t.Fatalf("unexpected ffprobe line: %q", l)
		}
		// ffprobe's csv writer emits struct fields alphabetically regardless
		// of -show_entries order: codec_name, then codec_type, then
		// nb_read_frames.
		codecName, codecType, nbReadFrames := fields[0], fields[1], fields[2]
		n, err := strconv.Atoi(nbReadFrames)
		if err != nil {
			t.Fatalf("parse nb_read_frames %q: %v", nbReadFrames, err)
		}
		switch codecType {
		case "video":
			sawVideo = true
			if codecName != "hevc" {
				t.Errorf("video codec_name = %q, want hevc", codecName)
			}
		case "audio":
			sawAudio = true
			audioFrames = n
			if codecName != "aac" {
				t.Errorf("audio codec_name = %q, want aac", codecName)
			}
		}
	}
	if !sawVideo {
		t.Fatal("ffprobe reported no video stream out of Run's own output")
	}
	if !sawAudio {
		t.Fatal("ffprobe reported no audio stream: Run did not carry audio to the hub")
	}
	if audioFrames != 56 {
		t.Errorf("ffprobe read %d audio frames, want 56 (the fixture's known AAC frame count)", audioFrames)
	}
}

// waitForHeader blocks until h.Header is set, the same wait
// TestRunSetsHeaderBeforeStartingToPublish uses, and returns it.
func waitForHeader(t *testing.T, h *hub.Hub) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hdr := h.Header(); hdr != nil {
			return hdr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("header was never set")
	return nil
}
