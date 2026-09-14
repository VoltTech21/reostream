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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
// still be reachable so someone can. A later task adds a claim screen and a
// private-source-address gate that close this off; do not read this as a
// general-purpose way to run the control page without a password.
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

	configPath := resolveConfigPath(*dataDir, *configFlag)

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
	if _, statErr := os.Stat(configPath); errors.Is(statErr, fs.ErrNotExist) {
		// No config file at all: this is a fresh install, not an error.
		// Start with an empty fleet and a claimable control page instead of
		// refusing to run, and keep saying so until something claims it,
		// per resolveConfigPath's doc comment.
		cfg = &config.Config{
			Listen: defaultListen,
			Control: &config.ControlConfig{
				Listen:          defaultControlListen,
				AllowNoPassword: true,
			},
		}
		msg := fmt.Sprintf("reostream: no config at %s, serving the setup page, not yet claimed", configPath)
		log.Print(msg)
		go func() {
			t := time.NewTicker(firstRunLogInterval)
			defer t.Stop()
			for {
				select {
				case <-supCtx.Done():
					return
				case <-t.C:
					log.Print(msg)
				}
			}
		}()
	} else {
		// The file exists (or Stat failed for some other reason, in which
		// case Load below will surface it): load it normally. A file that
		// exists and fails to parse or validate stays fatal here -- unlike
		// an absent file, this is a config somebody wrote, and silently
		// ignoring it in favor of defaults is how a fleet quietly runs
		// unconfigured without anyone noticing.
		c, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reostream: %v\n", err)
			os.Exit(1)
		}
		cfg = c
	}

	listen := cfg.Listen
	if *listenOverride != "" {
		listen = *listenOverride
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
			log.Fatalf("reostream: %v", err)
		}
		log.Printf("reostream: rtsp listening on %s", cfg.RTSP.Listen)
	}

	// The control page is a separate listener from the streaming one. The
	// streaming port stays unauthenticated because that is what a recorder
	// points at; this one holds camera credentials.
	var controlSrv *http.Server
	if cfg.Control != nil && cfg.Control.Listen != "" {
		ctl, err := control.New(control.Options{
			Password:        cfg.Control.Password,
			AllowNoPassword: cfg.Control.AllowNoPassword,
			Status:          srv,
			Logs:            logs,
			Hubs:            sup,
			StreamBase:      *streamBase,
			ConfigPath:      configPath,
			Supervisor:      sup,
		})
		if err != nil {
			log.Fatalf("reostream: control: %v", err)
		}
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
	if rtspSrv != nil {
		rtspSrv.Close()
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
	if controlSrv != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			controlSrv.Shutdown(ctx)
		}()
	}

	if err := runShutdown(cancelSup, runDone, httpSrv, runStopGrace, httpShutdownTimeout); err != nil {
		log.Printf("reostream: http shutdown: %v", err)
	}
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
// any of this. Only when it is empty does a fresh install's convention
// apply, <data>/config.toml, so starting the container with just -data (or
// its default, /data) set is enough to find or create a config without
// anyone having to know the flag exists.
func resolveConfigPath(dataDir, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(dataDir, "config.toml")
}
