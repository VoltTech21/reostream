// Command reostream serves Reolink camera video as MPEG-TS over HTTP for
// every camera listed in a config file, reconnecting failed streams with
// backoff and never running two connections to the same stream at once.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/control"
	"github.com/VoltTech21/reostream/internal/rtsp"
	"github.com/VoltTech21/reostream/internal/server"
	"github.com/VoltTech21/reostream/internal/stream"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// defaultListen is used when neither the config file nor -listen sets one.
const defaultListen = "0.0.0.0:8560"

// defaultControlListen is the control page's listen address for a
// synthesized first-run config, when no config file exists at all yet (see
// resolveConfigPath and the no-config branch in main). AllowNoPassword is
// correct ONLY in that unclaimed state: there is no operator-set password
// to check because nobody has configured anything yet, and the page must
// still be reachable so someone can. The control page closes this off
// itself: while no config exists it sends every route to its claim screen,
// which only accepts the one-time token printed below, and claiming writes
// a password into a real config and starts requiring it in this same
// process. Do not read this as a general-purpose way to run the control
// page without a password.
const defaultControlListen = "0.0.0.0:8562"

// firstRunLogInterval is how often main repeats the "not yet claimed"
// message while the daemon is running on a synthesized config: loud enough
// that it turns up in `docker logs` without anyone going looking for it.
const firstRunLogInterval = 60 * time.Second

// runStopGrace bounds how long shutdown waits for Supervisor.Run to return
// after its context is cancelled. Run does not return until every stream's
// stream-stop message has gone out, and that is what releases each camera's
// session; a shutdown that does not wait for it leaves every camera in the
// fleet, not just one, refusing its next connection for minutes. Every
// stream stops concurrently (one goroutine each), so this does not need to
// scale with the number of cameras, only with how long one stream-stop
// round trip takes.
const runStopGrace = 5 * time.Second

func main() {
	configFlag := flag.String("config", "", "path to the TOML config file (default: <data>/config.toml)")
	dataDir := flag.String("data", "/data", "data directory; holds config.toml when -config is not set")
	listenOverride := flag.String("listen", "", "HTTP listen address, overriding the config file's")
	streamBase := flag.String("stream-base", "", "browser reachable base URL for live tiles, for example http://10.0.0.2:8560 (empty means the control page's own same-origin /stream/ mount, which is the right default; only set this to point tiles at a different listener)")
	flag.Parse()

	d, err := startup(*configFlag, *dataDir, *listenOverride, *streamBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reostream: %v\n", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	// SIGHUP and SIGQUIT must be handled, not just SIGINT and SIGTERM: this
	// process is normally started over SSH, where a terminal disconnect or a
	// tmux detach sends SIGHUP, and Go's default action for an unhandled
	// SIGHUP or SIGQUIT terminates the process immediately with no defers
	// run. That skips every stream's stream-stop message, and the cameras
	// refuse new connections on those streams for minutes. This is not
	// hypothetical: it cost nine minutes of live camera footage during
	// testing on 2026-09-08.
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	sig := <-sigs
	log.Printf("reostream: got %s, shutting down", sig)

	// Close RTSP before the streams stop. A reader still attached when the
	// camera sessions are released would otherwise be served from a stream
	// that is being torn down underneath it.
	if d.rtspSrv != nil {
		d.rtspSrv.Close()
	}

	// Shut the control server down in its own goroutine, not inline here.
	// http.Server.Shutdown blocks until active connections finish and does
	// not cancel their request contexts, so a long-lived connection on the
	// control listener (the log stream Task 7 adds, or just a browser tab
	// left open on the page) would otherwise delay runShutdown's cancel()
	// below by up to its own timeout, and that cancel is what starts
	// releasing camera sessions. Running it concurrently means a slow
	// control-page client only delays the control listener's own shutdown,
	// never the start of camera session release.
	if d.controlSrv != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			d.controlSrv.Shutdown(ctx)
		}()
	}

	if err := runShutdown(d.cancelSup, d.runDone, d.httpSrv, runStopGrace, httpShutdownTimeout); err != nil {
		log.Printf("reostream: http shutdown: %v", err)
	}
}

// daemon holds everything startup assembles -- the resolved config, the
// supervisor, and every listener -- before main blocks on a shutdown
// signal. Factoring this out of main lets a test drive the real
// config-resolution-and-serve path (see firstrun_test.go) without main's
// signal handling, which stays untouched below.
type daemon struct {
	cfg        *config.Config
	sup        *supervisor.Supervisor
	rtspSrv    *rtsp.Server
	controlSrv *http.Server
	httpSrv    *http.Server
	supCtx     context.Context
	cancelSup  context.CancelFunc
	runDone    <-chan error
}

// startup resolves the config -- including the no-config first-run path --
// builds the supervisor and every listener exactly as main always has, and
// starts them, returning before anything blocks on a shutdown signal. An
// error here is always fatal in main: a config that exists and fails to
// parse or validate, an explicit -config pointing at nothing, or a listener
// that fails to start.
func startup(configFlag, dataDir, listenOverride, streamBase string) (*daemon, error) {
	explicitConfig := configFlag != ""
	configPath := resolveConfigPath(dataDir, configFlag)

	// Tee, not redirect: stderr keeps everything it had, so docker logs and
	// journald are unaffected, and the page reads the same lines from
	// memory. Set up logging before deciding whether a config exists, so
	// the first-run message below reaches both.
	logs := control.NewLogBuffer(2000)
	log.SetOutput(io.MultiWriter(os.Stderr, logs))

	// supCtx is created here, ahead of everything else that needs to stop
	// when the daemon shuts down, so the first-run logging loop below can
	// use it instead of inventing its own lifecycle.
	supCtx, cancelSup := context.WithCancel(context.Background())

	var cfg *config.Config
	// firstRun defers the "not yet claimed" logging until after the control
	// server exists, because the message carries that server's one-time
	// claim token and there is nowhere else to get one from. Everything
	// else about this branch is unchanged.
	firstRun := false
	if _, statErr := os.Stat(configPath); errors.Is(statErr, fs.ErrNotExist) {
		if explicitConfig {
			// An explicit -config that names a file which is not there is
			// not a fresh install, it is a broken deployment -- an
			// unmounted volume, a typo'd flag during a migration, a bind
			// mount that has not attached yet. Only the *default* path
			// gets the first-run treatment; a caller that named a path
			// stays exactly as fatal as it always was, so a redeploy that
			// loses its config crash-loops loudly instead of quietly
			// coming up with an unauthenticated control page.
			cancelSup()
			return nil, fmt.Errorf("config: %s: %w", configPath, statErr)
		}
		// No config file at all, and nobody asked for one by name: this is
		// a fresh install, not an error. Start with an empty fleet and a
		// claimable control page instead of refusing to run, and keep
		// saying so until something claims it.
		cfg = &config.Config{
			Listen: defaultListen,
			Control: &config.ControlConfig{
				Listen:          defaultControlListen,
				AllowNoPassword: true,
			},
		}
		firstRun = true
	} else {
		// The file exists (or Stat failed for some other reason, in which
		// case Load below will surface it): load it normally. A file that
		// exists and fails to parse or validate stays fatal here -- unlike
		// an absent file, this is a config somebody wrote, and silently
		// ignoring it in favor of defaults is how a fleet quietly runs
		// unconfigured without anyone noticing.
		c, err := config.Load(configPath)
		if err != nil {
			cancelSup()
			return nil, err
		}
		cfg = c
	}

	listen := cfg.Listen
	if listenOverride != "" {
		listen = listenOverride
	}
	if listen == "" {
		listen = defaultListen
	}

	sup := supervisor.New(cfg.Cameras, stream.Run)
	srv := server.New(sup)
	srv.SetSupervisor(sup)

	// RTSP is constructed only when the config asks for it. With no [rtsp]
	// section nothing here runs and every stream keeps a nil sink, which is
	// what the HTTP output has always seen.
	var rtspSrv *rtsp.Server
	if cfg.RTSP != nil {
		rtspSrv = rtsp.New(cfg.RTSP.Listen)
		sup.AttachSinks(func(camera, st string) stream.FrameSink {
			return rtspSrv.Add(rtsp.Path(camera, st))
		})
		if err := rtspSrv.Start(); err != nil {
			cancelSup()
			return nil, err
		}
		log.Printf("reostream: rtsp listening on %s", cfg.RTSP.Listen)
	}

	// The control page is a separate listener from the streaming one. The
	// streaming port stays unauthenticated because that is what a recorder
	// points at; this one holds camera credentials.
	var controlSrv *http.Server
	var claimToken string
	if cfg.Control != nil && cfg.Control.Listen != "" {
		ctl, err := control.New(control.Options{
			Password:        cfg.Control.Password,
			AllowNoPassword: cfg.Control.AllowNoPassword,
			Status:          srv,
			Logs:            logs,
			Hubs:            sup,
			StreamBase:      streamBase,
			ConfigPath:      configPath,
			Supervisor:      sup,
		})
		if err != nil {
			cancelSup()
			return nil, fmt.Errorf("control: %w", err)
		}
		claimToken = ctl.ClaimToken()
		controlSrv = &http.Server{Addr: cfg.Control.Listen, Handler: ctl.Handler()}
		// RegisterOnShutdown runs at the start of Shutdown, before it waits
		// on active connections, which is exactly when a live log stream
		// needs to be released: Shutdown blocks on active connections
		// without cancelling their request contexts, so a long-lived
		// stream needs its own signal to know to stop.
		controlSrv.RegisterOnShutdown(ctl.Close)
		go func() {
			log.Printf("reostream: control listening on %s", cfg.Control.Listen)
			if err := controlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("reostream: control: %v", err)
			}
		}()
	}

	// After the control server, so the message can carry its claim token,
	// and before anything blocks: a fresh install must say what it is and
	// keep saying it until something claims it.
	if firstRun {
		controlListen := ""
		if cfg.Control != nil {
			controlListen = cfg.Control.Listen
		}
		msg := firstRunMessage(configPath, controlListen, claimToken)
		log.Print(msg)
		go firstRunLoop(supCtx, configPath, msg)
	}

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler()}

	runDone := make(chan error, 1)
	go func() {
		runDone <- sup.Run(supCtx)
	}()

	go func() {
		log.Printf("reostream: listening on %s", listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("reostream: http: %v", err)
		}
	}()

	return &daemon{
		cfg:        cfg,
		sup:        sup,
		rtspSrv:    rtspSrv,
		controlSrv: controlSrv,
		httpSrv:    httpSrv,
		supCtx:     supCtx,
		cancelSup:  cancelSup,
		runDone:    runDone,
	}, nil
}

// firstRunMessage is the log block startup prints, and firstRunLoop
// repeats, while the daemon is running on a synthesized first-run config.
//
// It carries the one-time claim token, and the log is the ONLY place that
// token appears: it is never written to disk, never put in a response body
// or a URL, and never returned in an error. That is the whole design --
// what the token proves is that whoever has it can read this daemon's
// logs, which is what controlling the deployment actually looks like,
// unlike a source address (see internal/control/claimtoken.go).
//
// The token is held in memory only, so restarting an install that has not
// been claimed yet prints a different one and the old one stops working.
// The message says so, because an operator who restarts the container
// while following these instructions would otherwise be typing a dead
// token at a screen that only tells them it is wrong.
func firstRunMessage(configPath, controlListen, token string) string {
	if token == "" {
		// No control page was started, so there is nothing to claim and no
		// token to print. The shipped daemon never reaches this -- the
		// synthesized first-run config always names a control listener --
		// but a config that names none must still say what it is doing.
		return fmt.Sprintf("reostream: no config at %s, serving no control page, not yet claimed", configPath)
	}
	return fmt.Sprintf(`reostream: not yet claimed. To claim this install, open
  %s
and enter this token:

      %s

(The token is only shown here and only until claimed.)
(It is held in memory only: restarting reostream before it is claimed
prints a new one, and this one stops working. No config yet at %s.)`,
		claimURL(controlListen), token, configPath)
}

// claimURL is the address that message tells an operator to open. A listen
// address is a bind address, not a hostname: 0.0.0.0 and :: mean "every
// interface on this machine", and printing either back as something to type
// into a browser would be sending somebody to an address that does not
// exist. Those become a literal <host> placeholder, which is honest -- only
// the operator knows which of this machine's addresses they can reach it on
// -- while the port, the part they could not guess, is exact.
func claimURL(controlListen string) string {
	host, port, err := net.SplitHostPort(controlListen)
	if err != nil {
		// Not a host:port at all. Say what was configured rather than
		// inventing a URL around it.
		return fmt.Sprintf("the control page (listening on %s), at /claim", controlListen)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "<host>"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%s/claim", host, port)
}

// firstRunLoop repeats msg every firstRunLogInterval, loud
// enough that it turns up in `docker logs` without anyone going looking for
// it, until ctx is cancelled (the daemon is shutting down) or configPath
// exists (something -- the setup page, or an operator by hand -- has
// written a config there). It re-Stats configPath on every tick rather than
// trusting the absence it saw at startup: an operator can use the open page
// to write a real config while this loop is still running, and the message
// must stop being true the moment that happens, not wait for a restart.
//
// This only silences the log. Making the claim real in the running process
// -- requiring the new password on every route, with no restart -- is the
// control page's own job, done where the claim is handled; see
// internal/control/claim.go. This loop just stops saying something that has
// stopped being true.
func firstRunLoop(ctx context.Context, configPath, msg string) {
	t := time.NewTicker(firstRunLogInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if firstRunClaimed(configPath) {
				return
			}
			log.Print(msg)
		}
	}
}

// firstRunClaimed reports whether configPath now exists -- meaning
// something has written a config since the daemon started on a synthesized
// one, so firstRunMessage is no longer true and firstRunLoop should stop
// repeating it.
func firstRunClaimed(configPath string) bool {
	_, err := os.Stat(configPath)
	return !errors.Is(err, fs.ErrNotExist)
}

// httpShutdownTimeout bounds http.Server.Shutdown itself, once the stream
// side has already stopped or been given up on.
const httpShutdownTimeout = 5 * time.Second

// httpShutdowner is the subset of *http.Server that runShutdown needs, so a
// test can substitute a fake and check the shutdown sequence without
// opening a real listener.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
}

// runShutdown performs the cancel-then-wait-then-shutdown sequence: cancel
// the supervisor's context, wait up to grace for it to actually finish, then
// shut down HTTP.
//
// This order is not incidental. Supervisor.Run does not return until every
// stream's stream-stop message has gone out, which is what releases each
// camera's session; shutting down HTTP before that, or not waiting for Run
// at all, buys nothing and risks leaving every camera in the fleet refusing
// its next connection for minutes. Getting this backwards, or skipping the
// wait, cost nine minutes of live camera footage during testing on
// 2026-09-08. Pulling it out of main into its own function is what lets
// that fact be checked by a test instead of only asserted in a comment.
func runShutdown(cancel context.CancelFunc, runDone <-chan error, srv httpShutdowner, grace, httpTimeout time.Duration) error {
	cancel()
	select {
	case <-runDone:
	case <-time.After(grace):
		log.Printf("reostream: streams did not all stop within %s, shutting down anyway", grace)
	}

	ctx, cancelShutdown := context.WithTimeout(context.Background(), httpTimeout)
	defer cancelShutdown()
	return srv.Shutdown(ctx)
}

// resolveConfigPath decides which config file main should try to load. An
// explicit -config override always wins: an existing deployment that
// already passes -config must keep working exactly as before, unchanged by
// any of this -- including staying fatal, not falling back to a fresh
// first-run config, when the named file is not there (see startup's use of
// explicitConfig, which is what actually keeps that promise; this function
// only chooses the path, not what happens when it is missing). Only when
// override is empty does a fresh install's convention apply,
// <data>/config.toml, so starting the container with just -data (or its
// default, /data) set is enough to find or create a config without anyone
// having to know the flag exists.
func resolveConfigPath(dataDir, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(dataDir, "config.toml")
}
