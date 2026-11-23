package rtsp

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

// Readers counts a reader from PLAY to the end of its session, so a
// battery camera with wake_on_rtsp wakes for it and sleeps after (#43). A
// session that set up and closed without playing used to be released
// anyway, taking the count from a reader still playing, whose stream then
// stopped being packetized and whose camera would go back to sleep.
func TestReadersCountsOnlyPlayingSessions(t *testing.T) {
	frames := captureFrames(t)
	srv := New("127.0.0.1:8554")
	st := srv.Add("cam")
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Close)
	feedUntilReady(t, st, frames)

	u, err := base.ParseURL("rtsp://" + srv.Addr() + "/cam")
	if err != nil {
		t.Fatal(err)
	}
	open := func() *gortsplib.Client {
		tr := gortsplib.ProtocolTCP
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tr}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			t.Fatal(err)
		}
		return c
	}
	waitReaders := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for srv.Readers("cam") != want {
			if time.Now().After(deadline) {
				t.Fatalf("%d readers, want %d", srv.Readers("cam"), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	player := open()
	defer player.Close()
	if n := srv.Readers("cam"); n != 0 {
		t.Fatalf("%d readers before PLAY", n)
	}
	if _, err := player.Play(nil); err != nil {
		t.Fatal(err)
	}
	if n := srv.Readers("/cam/"); n != 1 {
		t.Fatalf("%d readers after PLAY; want 1", n)
	}

	// Set up, never played, gone.
	open().Close()
	time.Sleep(200 * time.Millisecond)
	waitReaders(1)

	player.Close()
	waitReaders(0)
	if n := srv.Readers("nothing"); n != 0 {
		t.Fatalf("%d readers on a path that is not served", n)
	}
}
