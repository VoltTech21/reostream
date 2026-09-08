package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
		h.Publish([]byte("PAYLOAD"))
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
