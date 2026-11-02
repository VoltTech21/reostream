package supervisor

import (
	"context"
	"testing"

	"github.com/VoltTech21/reostream/internal/config"
)

// A changed talk_gain_db reaches the next talk session and restarts no
// stream.
func TestReloadUpdatesTalkGainWithoutARestart(t *testing.T) {
	c := &runCounter{calls: map[string]int{}}
	cams := []config.Camera{{Name: "a", Address: "1.1.1.1", Streams: []string{"main"}, TalkGainDB: -6}}
	s := New(cams, c.runner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, func() bool { return c.count("a/main") == 1 })
	if g := s.TalkGainDB("a"); g != -6 {
		t.Fatalf("talk gain %d at start, want -6", g)
	}
	cams[0].TalkGainDB = -12
	res, err := s.Reload(cams)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restarted != 0 || res.Unchanged != 1 {
		t.Errorf("got %+v, want the stream left alone", res)
	}
	if g := s.TalkGainDB("a"); g != -12 {
		t.Errorf("talk gain %d after the reload, want -12", g)
	}
}
