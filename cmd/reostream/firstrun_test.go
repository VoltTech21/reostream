package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/control"
)

func TestConfigPathPrefersAnExplicitOverride(t *testing.T) {
	// An existing deployment passes -config and must keep working
	// unchanged; only a fresh install gets the data directory convention.
	if got := resolveConfigPath("/data", "/etc/reostream/config.toml"); got != "/etc/reostream/config.toml" {
		t.Fatalf("got %q, want the override", got)
	}
	if got := resolveConfigPath("/data", ""); got != "/data/config.toml" {
		t.Fatalf("got %q, want the data directory default", got)
	}
}

func TestStartupWithAnExplicitMissingConfigStaysFatal(t *testing.T) {
	// A -config that names a file which is not there is a broken
	// deployment (unmounted volume, typo'd flag, migration in progress),
	// not a fresh install: it must still refuse to start, exactly as it
	// always has, rather than quietly booting an unauthenticated control
	// page on the default first-run config.
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.toml")
	if _, err := startup(missing, dir, "", "", ""); err == nil {
		t.Fatal("expected an error for an explicit -config pointing at a missing file")
	}
}

func TestStartupWithNoConfigSynthesizesAndServes(t *testing.T) {
	// The whole point of the no-config path: with nothing at
	// <data>/config.toml and no -config given, the daemon must actually
	// come up -- an empty fleet, and a control listener that answers --
	// not merely fail to crash.
	dir := t.TempDir()
	d, err := startup("", dir, "127.0.0.1:0", "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.controlSrv != nil {
			d.controlSrv.Close()
		}
		d.httpSrv.Close()
	})

	if len(d.cfg.Cameras) != 0 {
		t.Fatalf("synthesized config has %d cameras, want 0", len(d.cfg.Cameras))
	}
	if d.controlSrv == nil {
		t.Fatal("startup did not start a control server for a synthesized config")
	}

	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Get("http://" + d.controlLn.Addr().String() + "/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control listener never answered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("control page returned %d", resp.StatusCode)
	}
}

func TestFirstRunClaimedTracksTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if firstRunClaimed(path) {
		t.Fatal("claimed before the file exists")
	}
	if err := os.WriteFile(path, []byte(`listen = "0.0.0.0:8560"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !firstRunClaimed(path) {
		t.Fatal("not claimed after the file was written")
	}
}

// The first-run block on stderr is the ONLY place the claim token appears,
// so its shape is load-bearing: a person has to find it in `docker logs` and
// type it into a browser.
func TestFirstRunMessageCarriesTheTokenAndTheURL(t *testing.T) {
	msg := firstRunMessage("/data/config.toml", "0.0.0.0:8562", "K7M2-QX94-B3TD-9WFH")
	for _, want := range []string{
		"reostream: not yet claimed",
		// A bind address is not a hostname: only the operator knows which
		// of this machine's addresses they can reach it on, but the port
		// is exact.
		"http://<host>:8562/claim",
		"and enter this token:",
		"K7M2-QX94-B3TD-9WFH",
		"only shown here and only until claimed",
		// Held in memory only, so a restart invalidates it. Saying so is
		// what keeps an operator from typing a dead token at a screen that
		// only tells them it is wrong.
		"restarting reostream before it is claimed",
		"/data/config.toml",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the first-run message never says %q:\n%s", want, msg)
		}
	}
}

// The first-run block must not go through the log package: log's output is
// teed into the in-memory buffer the Logs page serves, and the block carries
// the claim token. Stderr still gets it, which is where `docker logs` reads.
func TestTheFirstRunBlockDoesNotReachTheLogBuffer(t *testing.T) {
	logs := control.NewLogBuffer(100)
	saved := log.Writer()
	log.SetOutput(io.MultiWriter(io.Discard, logs))
	t.Cleanup(func() { log.SetOutput(saved) })

	msg := firstRunMessage("/data/config.toml", "0.0.0.0:8562", "K7M2-QX94-B3TD-9WFH")
	printFirstRun(msg)
	// Something that DOES go through log, to prove the buffer is wired up
	// and the absence above means something.
	log.Print("reostream: a line that is not a secret")

	var buffered string
	for _, line := range logs.Lines() {
		buffered += line + "\n"
	}
	if strings.Contains(buffered, "K7M2-QX94-B3TD-9WFH") {
		t.Fatalf("the claim token reached the log buffer:\n%s", buffered)
	}
	if !strings.Contains(buffered, "not a secret") {
		t.Fatalf("the log buffer never saw an ordinary log line, so this test proves nothing:\n%s", buffered)
	}
}

func TestClaimURLNamesTheHostItCan(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8562":       "http://<host>:8562/claim",
		":8562":              "http://<host>:8562/claim",
		"[::]:8562":          "http://<host>:8562/claim",
		"192.168.1.5:8562":   "http://192.168.1.5:8562/claim",
		"[2001:db8::1]:8562": "http://[2001:db8::1]:8562/claim",
	}
	for listen, want := range cases {
		if got := claimURL(listen); got != want {
			t.Errorf("claimURL(%q) = %q, want %q", listen, got, want)
		}
	}
	// Not a host:port at all: say what was configured rather than invent a
	// URL around it.
	if got := claimURL("nonsense"); !strings.Contains(got, "nonsense") {
		t.Errorf("claimURL(%q) = %q, want it to name what was configured", "nonsense", got)
	}
}

// The token is held in memory only. A fresh install that starts, prints a
// token and is restarted before anyone claims it must leave nothing behind
// on disk for the next process to read -- and the next process must print a
// different token.
func TestTheClaimTokenIsNeverWrittenToDisk(t *testing.T) {
	dir := t.TempDir()
	tokens := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		ctl, err := control.New(control.Options{
			AllowNoPassword: true,
			ConfigPath:      filepath.Join(dir, "config.toml"),
		})
		if err != nil {
			t.Fatal(err)
		}
		tok := ctl.ClaimToken()
		if tok == "" {
			t.Fatal("an unclaimed install has no claim token")
		}
		tokens = append(tokens, tok)
		ctl.Close()
	}
	if tokens[0] == tokens[1] {
		t.Fatal("a restart while unclaimed reused the token")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range tokens {
			if strings.Contains(string(b), tok) ||
				strings.Contains(string(b), strings.ReplaceAll(tok, "-", "")) {
				t.Fatalf("%s holds a claim token", e.Name())
			}
		}
	}
}
