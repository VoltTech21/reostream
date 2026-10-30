package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
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
	if _, err := startup(missing, dir, "", "", "", "", pageOptions{}); err == nil {
		t.Fatal("expected an error for an explicit -config pointing at a missing file")
	}
}

func TestStartupWithNoConfigSynthesizesAndServes(t *testing.T) {
	// The whole point of the no-config path: with nothing at
	// <data>/config.toml and no -config given, the daemon must actually
	// come up -- an empty fleet, and a control listener that answers --
	// not merely fail to crash.
	dir := t.TempDir()
	d, err := startup("", dir, "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "", pageOptions{})
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.controlSrv != nil {
			d.controlSrv.Close()
		}
		if d.rtspSrv != nil {
			d.rtspSrv.Close()
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

// TestTheClaimMessageFollowsTheControlListenOverride pins a bug found by
// running the released binary the way the README tells a stranger to run
// it. -control-listen moved the listener, but the claim block and the
// startup log line both still read cfg.Control.Listen, so a first run on
// any port but 8562 printed a URL that answers nothing, and that URL is
// the only way in, since the token is shown nowhere else.
func TestTheClaimMessageFollowsTheControlListenOverride(t *testing.T) {
	dir := t.TempDir()
	d, err := startup("", dir, "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "", pageOptions{})
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.controlSrv != nil {
			d.controlSrv.Close()
		}
		if d.rtspSrv != nil {
			d.rtspSrv.Close()
		}
		d.httpSrv.Close()
	})

	if d.firstRunMsg == "" {
		t.Fatal("a fresh data directory produced no first-run message")
	}
	// ":0" means the kernel picked the port, so the bound address is the
	// only correct answer and 8562 is definitely the wrong one.
	bound := d.controlLn.Addr().String()
	if !strings.Contains(d.firstRunMsg, bound) {
		t.Fatalf("the claim message does not name the address the page is on (%s):\n%s", bound, d.firstRunMsg)
	}
	if strings.Contains(d.firstRunMsg, ":8562") {
		t.Fatalf("the claim message names the default port despite an override:\n%s", d.firstRunMsg)
	}
}

// -rtsp-listen moves RTSP the way -listen and -control-listen move theirs
// (issue #8), and turns RTSP on for a config with no [rtsp] section.
func TestRTSPListenFlagMovesRTSP(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("listen = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := startup(cfg, dir, "127.0.0.1:0", addr, "", "", pageOptions{})
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
	if d.rtspSrv == nil || d.cfg.RTSP == nil || d.cfg.RTSP.Listen != addr {
		t.Fatalf("RTSP not on %s: %+v", addr, d.cfg.RTSP)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listening on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Startup with a battery camera whose last picture was kept: the presence
// stream reads that picture while the supervisor is setting up its sinks.
// It once did so through the supervisor's own lock and deadlocked the
// whole daemon at start.
func TestStartupWithABatteryCameraAndAKeptStill(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.MkdirAll(filepath.Join(dir, "stills"), 0o755)
	os.WriteFile(filepath.Join(dir, "stills", "porch_main.still"), []byte("H264\n\x00\x00\x00\x01\x67\x64\x00\x1f\x00\x00\x00\x01\x68\xee\x00\x00\x00\x01\x65\x88"), 0o644)
	os.WriteFile(cfg, []byte("listen = \"127.0.0.1:0\"\n[rtsp]\nlisten = \"127.0.0.1:0\"\n[[camera]]\nname = \"porch\"\naddress = \"192.0.2.20\"\npassword = \"x\"\nuid = \"95270000TESTUID1\"\n"), 0o600)
	done := make(chan *daemon, 1)
	go func() {
		d, err := startup(cfg, dir, "127.0.0.1:0", "127.0.0.1:0", "", "", pageOptions{})
		if err != nil {
			t.Errorf("startup: %v", err)
		}
		done <- d
	}()
	select {
	case d := <-done:
		if d != nil {
			d.cancelSup()
			if d.rtspSrv != nil {
				d.rtspSrv.Close()
			}
			d.httpSrv.Close()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup hung")
	}
}

func TestAClaimSavesTheListenFlagsTheDaemonWasStartedWith(t *testing.T) {
	// #9, end to end: the flags main resolved must reach the claim, so the
	// config it writes matches how the daemon is really running.
	dir := t.TempDir()
	d, err := startup("", dir, "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "", pageOptions{})
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	t.Cleanup(func() {
		d.cancelSup()
		if d.controlSrv != nil {
			d.controlSrv.Close()
		}
		if d.rtspSrv != nil {
			d.rtspSrv.Close()
		}
		d.httpSrv.Close()
	})

	token := regexp.MustCompile(`[A-Z0-9]{4}(-[A-Z0-9]{4}){3}`).FindString(d.firstRunMsg)
	if token == "" {
		t.Fatalf("no claim token in the first-run message:\n%s", d.firstRunMsg)
	}
	form := url.Values{"token": {token}, "password": {"correct-horse-battery"}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm("http://"+d.controlLn.Addr().String()+"/claim", form)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim returned %d", resp.StatusCode)
	}

	cfg, err := config.Load(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("claimed config does not load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:0" || cfg.RTSP == nil || cfg.RTSP.Listen != "127.0.0.1:0" ||
		cfg.Control == nil || cfg.Control.Listen != "127.0.0.1:0" {
		t.Fatalf("claimed config did not keep the flags: listen=%q rtsp=%+v control=%+v", cfg.Listen, cfg.RTSP, cfg.Control)
	}
}
