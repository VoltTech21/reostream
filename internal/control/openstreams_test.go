package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// With no [rtsp] login anyone on the network can watch the streams and
// talk through a camera, so the pages that hand out stream addresses say
// so; with a login they do not.
func TestStreamPagesWarnWhenStreamsHaveNoLogin(t *testing.T) {
	for _, tc := range []struct {
		extra string
		warn  bool
	}{
		{"", true},
		{"\n[rtsp]\nlisten = \"0.0.0.0:8561\"\nusername = \"viewer\"\npassword = \"secret\"\n", false},
	} {
		path := writeTestConfig(t)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, tc.extra...), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newTestServer(t, Options{AllowNoPassword: true, ConfigPath: path})
		ts := httptest.NewServer(s.Handler())
		for _, page := range []string{"/setup/urls", "/settings/nvr"} {
			resp, err := http.Get(ts.URL + page)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if got := strings.Contains(string(body), `id="open-streams"`); got != tc.warn {
				t.Errorf("%s with %q: warning shown = %v, want %v", page, tc.extra, got, tc.warn)
			}
		}
		ts.Close()
	}
}
