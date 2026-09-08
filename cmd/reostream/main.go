// Command reostream serves Reolink camera video as MPEG-TS over HTTP for
// every camera listed in a config file, reconnecting failed streams with
// backoff and never running two connections to the same stream at once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/server"
	"github.com/VoltTech21/reostream/internal/stream"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// defaultListen is used when neither the config file nor -listen sets one.
const defaultListen = "0.0.0.0:8560"

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
	configPath := flag.String("config", "", "path to the TOML config file")
	listenOverride := flag.String("listen", "", "HTTP listen address, overriding the config file's")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "reostream: -config is required")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reostream: %v\n", err)
		os.Exit(1)
	}

	listen := cfg.Listen
	if *listenOverride != "" {
		listen = *listenOverride
	}
	if listen == "" {
		listen = defaultListen
	}

	sup := supervisor.New(cfg.Cameras, stream.Run)
	srv := server.New(sup.Hubs())
	srv.SetSupervisor(sup)

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler()}

	supCtx, cancelSup := context.WithCancel(context.Background())
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

	// Cancel first, then wait for Run to actually return, then shut down
	// HTTP: that order, not the reverse. Cancelling releases every camera's
	// session by sending its stream-stop message; shutting down HTTP first
	// would drop clients without buying anything, since a hub is closed by
	// the supervisor itself the moment its stream exits for good (see
	// internal/supervisor's runStream), and closing HTTP before that leaves
	// nothing achieved but a slower exit.
	cancelSup()
	select {
	case <-runDone:
	case <-time.After(runStopGrace):
		log.Printf("reostream: streams did not all stop within %s, shutting down anyway", runStopGrace)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("reostream: http shutdown: %v", err)
	}
}
