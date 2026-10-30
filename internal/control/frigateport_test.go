package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/alarms"
)

// A tester gave reostream Frigate's https://<ip>:8971 web address, and every
// event failed on the certificate with nothing saying what to use instead.
func TestFrigateWebAddressSaysToUseTheAPIPort(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	f := &frigateEvents{base: srv.URL, open: map[string]string{}}
	got := f.change(context.Background(), alarms.Change{Camera: "patio", Kind: "animal", On: true, Time: time.Now()})
	if !strings.Contains(got, "x509") || !strings.Contains(got, ":5000") {
		t.Errorf("change = %q, want the certificate error and the port 5000 hint", got)
	}
}

// The same tester then used port 5000, which Frigate's Docker setup had not
// published, and got a bare "connection refused".
func TestFrigatePortNotPublishedSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()
	f := &frigateEvents{base: addr, open: map[string]string{}}
	got := f.change(context.Background(), alarms.Change{Camera: "patio", Kind: "animal", On: true, Time: time.Now()})
	if !strings.Contains(got, "connection refused") || !strings.Contains(got, "5000:5000") {
		t.Errorf("change = %q, want the refusal and how to publish port 5000", got)
	}
}
