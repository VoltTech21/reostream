package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

// freeAddr is a loopback address nothing is listening on, for a listener
// whose address the test has to know before startup binds it.
func freeAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return probe.Addr().String()
}

// The [rtsp] login reaches both video listeners the daemon starts, and
// leaves the health check open.
func TestTheRTSPLoginGuardsBothVideoOutputs(t *testing.T) {
	httpAddr, rtspAddr := freeAddr(t), freeAddr(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	text := "listen = \"" + httpAddr + "\"\n[rtsp]\nlisten = \"" + rtspAddr + "\"\nusername = \"frigate\"\npassword = \"s3cret\"\n"
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := startup(cfg, dir, "", "", "", "", pageOptions{})
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.rtspSrv != nil {
			d.rtspSrv.Close()
		}
		d.httpSrv.Close()
	})

	status := func(path, user, pass string) int {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			req, _ := http.NewRequest(http.MethodGet, "http://"+httpAddr+path, nil)
			if user != "" {
				req.SetBasicAuth(user, pass)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET %s: %v", path, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if got := status("/cam.ts", "", ""); got != http.StatusUnauthorized {
		t.Errorf("a stream with no login: %d, want 401", got)
	}
	// No such camera, so past the login is a 404.
	if got := status("/cam.ts", "frigate", "s3cret"); got != http.StatusNotFound {
		t.Errorf("a stream with the login: %d, want 404 for a camera that does not exist", got)
	}
	if got := status("/api/status", "", ""); got != http.StatusOK {
		t.Errorf("the health check with no login: %d, want 200", got)
	}

	u, err := base.ParseURL("rtsp://" + rtspAddr + "/cam")
	if err != nil {
		t.Fatal(err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.Describe(u); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("RTSP DESCRIBE with no login: %v, want a 401", err)
	}
}
