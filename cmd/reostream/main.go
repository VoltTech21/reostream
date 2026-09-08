// Command reostream serves Reolink camera video as MPEG-TS over HTTP.
//
// This is the single camera form. Config file, supervisor, reconnection,
// audio, status and metrics come next; see docs/plans/.
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

	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/server"
	"github.com/VoltTech21/reostream/internal/stream"
)

// streamStopGrace bounds how long shutdown waits for stream.Run to return
// after its context is cancelled. Run's own Close sends the stream-stop
// message that releases the camera's session; a shutdown that does not wait
// for it leaves the camera refusing the next connection for minutes, which
// is worse than a few extra seconds of process exit time.
const streamStopGrace = 5 * time.Second

func main() {
	listen := flag.String("listen", "0.0.0.0:8560", "HTTP listen address")
	name := flag.String("name", "", "stream name, served at /<name>.ts")
	address := flag.String("address", "", "camera address, host or host:port")
	username := flag.String("username", "admin", "camera username")
	password := flag.String("password", "", "camera password (visible in `ps` and shell history to any local user; set REOSTREAM_PASSWORD instead where that matters)")
	streamName := flag.String("stream", "main", "camera stream: main, sub or extern")
	buffer := flag.Int("buffer", 64, "subscriber buffer size, in TS chunks")
	flag.Parse()

	if *name == "" || *address == "" {
		fmt.Fprintln(os.Stderr, "reostream: -name and -address are required")
		os.Exit(2)
	}

	// The flag is the only way to set a password today, but it lands in `ps`
	// output and shell history. The config file in the next plan supersedes
	// this; until then, an unset flag falls back to the environment, which
	// neither of those exposes.
	if *password == "" {
		*password = os.Getenv("REOSTREAM_PASSWORD")
	}

	h := hub.New(*buffer)
	srv := server.New(map[string]*hub.Hub{*name: h})

	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler()}

	streamCtx, cancelStream := context.WithCancel(context.Background())
	cfg := stream.Config{
		Name:     *name,
		Address:  *address,
		Username: *username,
		Password: *password,
		Stream:   *streamName,
	}

	runDone := make(chan error, 1)
	go func() {
		log.Printf("reostream: %s: dialing %s", cfg.Name, cfg.Address)
		err := stream.Run(streamCtx, cfg, h)
		log.Printf("reostream: %s: stream stopped: %v", cfg.Name, err)
		runDone <- err
	}()

	go func() {
		log.Printf("reostream: %s: listening on %s", cfg.Name, *listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("reostream: http: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	// SIGHUP and SIGQUIT must be handled, not just SIGINT and SIGTERM: this
	// process is normally started over SSH, where a terminal disconnect or a
	// tmux detach sends SIGHUP, and Go's default action for an unhandled
	// SIGHUP or SIGQUIT terminates the process immediately with no defers
	// run. That skips conn.Close(), so the stream-stop message that releases
	// the camera's session is never sent and the camera refuses new
	// connections on that stream for minutes. This is not hypothetical: it
	// cost nine minutes of live camera footage during testing on 2026-09-08.
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

	// Wait for whichever comes first: an operator's shutdown signal, or the
	// stream ending on its own. The second case is a failed dial, a bad
	// password, or an unknown stream name; there is no retry (that is the
	// supervisor's job, not this command's), so once the stream is gone this
	// process has nothing left to serve. Without this branch it would sit
	// here answering HTTP with a permanently silent hub until someone killed
	// it, and exit 0 when they did, which makes a failed start look exactly
	// like a healthy one to a process manager or to a human reading the log.
	//
	// failed carries the stream's error only when it ended on its own, before
	// any shutdown was requested. A signalled shutdown also makes Run return
	// an error (ctx.Err(), context.Canceled), but that is the expected,
	// requested outcome, not a failure to report as one.
	var failed error
	select {
	case sig := <-sigs:
		log.Printf("reostream: %s: got %s, shutting down", cfg.Name, sig)
		cancelStream()
		select {
		case <-runDone:
		case <-time.After(streamStopGrace):
			log.Printf("reostream: %s: stream did not stop within %s, exiting anyway", cfg.Name, streamStopGrace)
		}
	case failed = <-runDone:
		cancelStream() // release the context; the stream is already gone.
	}

	// http.Server.Shutdown waits for in-flight handlers to return on their
	// own rather than forcing them closed, and the streaming handler in
	// internal/server only returns when its request context is cancelled or
	// the hub drops it. h.Close() drops every subscriber immediately, so a
	// live client no longer makes Shutdown wait out the timeout below.
	h.Close()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("reostream: http shutdown: %v", err)
	}

	if failed != nil {
		fmt.Fprintf(os.Stderr, "reostream: %s: %v\n", cfg.Name, failed)
		os.Exit(1)
	}
}
