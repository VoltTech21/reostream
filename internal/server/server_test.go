package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/hub"
)

func TestUnknownStreamIs404(t *testing.T) {
	s := New(map[string]*hub.Hub{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/nope.ts", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestStreamServesTSWithTheRightContentType(t *testing.T) {
	h := hub.New(8)
	h.SetHeader([]byte("HEADER"))
	s := New(map[string]*hub.Hub{"cam": h})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/cam.ts", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type %q, want video/mp2t", ct)
	}

	// The cached header must arrive first so the client's decoder can start,
	// then live data.
	go func() {
		time.Sleep(50 * time.Millisecond)
		// startsKeyframe true: this test is about header/payload ordering
		// and content type, not the hub's keyframe sync gate.
		h.PublishKey([]byte("PAYLOAD"), true)
	}()

	buf := make([]byte, 13)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "HEADERPAYLOAD" {
		t.Fatalf("got %q, want the header then the payload", buf)
	}
}

func TestClientDisconnectUnsubscribes(t *testing.T) {
	h := hub.New(8)
	s := New(map[string]*hub.Hub{"cam": h})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/cam.ts", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.Clients() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.Clients() != 1 {
		t.Fatalf("Clients = %d, want 1 while connected", h.Clients())
	}
	cancel()
	resp.Body.Close()

	deadline = time.Now().Add(2 * time.Second)
	for h.Clients() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.Clients() != 0 {
		t.Fatalf("Clients = %d after disconnect, want 0: the subscription leaked", h.Clients())
	}
}

// TestFlatURLFormsResolveToHubKeys covers the deviation found during the
// 2026-09-08 live test: the supervisor keys hubs "<camera>/<stream>", and
// the server used to route on that key directly, so the only working URL was
// "/lounge/main.ts" while the README, and therefore every recorder config
// written against this project, said "/lounge.ts".
func TestFlatURLFormsResolveToHubKeys(t *testing.T) {
	main, sub, extern := hub.New(1), hub.New(1), hub.New(1)
	s := New(map[string]*hub.Hub{
		"lounge/main":   main,
		"lounge/sub":    sub,
		"lounge/extern": extern,
	})
	for _, tc := range []struct {
		path string
		want *hub.Hub
	}{
		{"/lounge.ts", main},
		{"/lounge_sub.ts", sub},
		{"/lounge_extern.ts", extern},
		{"/lounge/main.ts", main},
		{"/lounge/sub.ts", sub},
	} {
		got, ok := s.lookup(strings.TrimSuffix(strings.TrimPrefix(tc.path, "/"), ".ts"))
		if !ok {
			t.Errorf("%s did not resolve to any stream", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("%s resolved to the wrong hub", tc.path)
		}
	}
	if _, ok := s.lookup("lounge_nope"); ok {
		t.Error("an unknown suffix resolved to a stream")
	}
}

// A camera whose name ends in one of the stream suffixes resolves to itself,
// not to a suffix reading of its name.
func TestARealCameraNameBeatsASuffixReading(t *testing.T) {
	real, other := hub.New(1), hub.New(1)
	s := New(map[string]*hub.Hub{"gate_sub/main": real, "gate/sub": other})
	got, ok := s.lookup("gate_sub")
	if !ok || got != real {
		t.Error("gate_sub resolved to gate's substream rather than the camera named gate_sub")
	}
}
