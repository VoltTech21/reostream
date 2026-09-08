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
	password := flag.String("password", "", "camera password")
	streamName := flag.String("stream", "main", "camera stream: main, sub or extern")
	buffer := flag.Int("buffer", 64, "subscriber buffer size, in TS chunks")
	flag.Parse()

	if *name == "" || *address == "" {
		fmt.Fprintln(os.Stderr, "reostream: -name and -address are required")
		os.Exit(2)
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
		log.Printf("reostream: %s: connecting to %s", cfg.Name, cfg.Address)
		err := stream.Run(streamCtx, cfg, h)
		log.Printf("reostream: %s: disconnected: %v", cfg.Name, err)
		runDone <- err
	}()

	go func() {
		log.Printf("reostream: %s: listening on %s", cfg.Name, *listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("reostream: http: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Printf("reostream: %s: shutting down", cfg.Name)

	// Cancel the stream first and wait for it to actually finish, so its
	// Close has a chance to send the stream-stop message before the process
	// exits. Shutting the HTTP server down first would only stop serving
	// clients; it does nothing to release the camera's session.
	cancelStream()
	select {
	case <-runDone:
	case <-time.After(streamStopGrace):
		log.Printf("reostream: %s: stream did not stop within %s, exiting anyway", cfg.Name, streamStopGrace)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("reostream: http shutdown: %v", err)
	}
}
