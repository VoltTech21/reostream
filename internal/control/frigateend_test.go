package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/alarms"
)

// #26: Frigate answers 404 to ending an event it has not saved yet.
func TestFrigateEndIsAskedAgainUntilTheEventExists(t *testing.T) {
	old := frigateEndRetries
	frigateEndRetries = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { frigateEndRetries = old }()

	var mu sync.Mutex
	ends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"event_id":"ev1"}`))
			return
		}
		ends++
		if ends < 3 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"message":"Event not found."}`))
			return
		}
		w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	f := &frigateEvents{base: srv.URL, open: map[string]string{}}
	ctx := context.Background()
	if note := f.change(ctx, alarms.Change{Camera: "porch", Kind: "person", On: true, Time: time.Now()}); note != "" {
		t.Fatal(note)
	}
	if note := f.change(ctx, alarms.Change{Camera: "porch", Kind: "person", On: false, Time: time.Now()}); note != "" {
		t.Fatalf("end not retried: %s", note)
	}
	if ends != 3 {
		t.Fatalf("asked to end %d times, want 3", ends)
	}
}

func TestFrigateEndGivesUpOnAnEventThatNeverAppears(t *testing.T) {
	old := frigateEndRetries
	frigateEndRetries = []time.Duration{time.Millisecond}
	defer func() { frigateEndRetries = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Write([]byte(`{"event_id":"ev1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := &frigateEvents{base: srv.URL, open: map[string]string{}}
	ctx := context.Background()
	f.change(ctx, alarms.Change{Camera: "porch", Kind: "person", On: true, Time: time.Now()})
	if note := f.change(ctx, alarms.Change{Camera: "porch", Kind: "person", On: false, Time: time.Now()}); note != "frigate end: 404" {
		t.Fatalf("note %q", note)
	}
}
