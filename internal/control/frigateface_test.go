package control

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/alarms"
)

// A person whose face the camera also detects is one Frigate person event,
// whichever is seen first, ended when the last of them goes (fake hardware,
// 10-30, bug 6: it was two overlapping person events).
func TestFaceAndPersonAreOneFrigateEvent(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			n++
			fmt.Fprintf(w, `{"event_id":"ev%d"}`, n)
		}
	}))
	defer srv.Close()
	f := &frigateEvents{base: srv.URL, open: map[string]string{}}
	send := func(kind string, on bool) {
		if note := f.change(context.Background(), alarms.Change{Camera: "porch", Kind: kind, On: on, Time: time.Now()}); note != "" {
			t.Fatalf("%s %v: %s", kind, on, note)
		}
	}
	send("person", true)
	send("face", true)
	send("face", false)
	send("person", false)
	send("face", true)
	send("person", true)
	send("person", false)
	send("face", false)
	want := "POST /api/events/porch/person/create, PUT /api/events/ev1/end, POST /api/events/porch/person/create, PUT /api/events/ev2/end"
	if got := strings.Join(calls, ", "); got != want {
		t.Errorf("Frigate got\n%s\nwant\n%s", got, want)
	}
}
