package control_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/server"
)

type fixedStatus map[string]server.StreamStatus

func (f fixedStatus) StreamStats() map[string]server.StreamStatus { return f }

func TestDashboardShowsEveryStreamAndItsError(t *testing.T) {
	ts := newTestServer(t, control.Options{
		AllowNoPassword: true,
		Status: fixedStatus{
			"driveway/main": {Connected: true, Streaming: true, FPS: 24.9},
			"gate/sub":      {Connected: false, LastError: "dial tcp: refused"},
		},
	})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	for _, want := range []string{"driveway/main", "gate/sub", "dial tcp: refused"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard does not mention %q", want)
		}
	}
}
