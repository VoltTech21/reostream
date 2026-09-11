package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/VoltTech21/reostream/internal/camctl"
)

// defaultServeListen and defaultServeConfig are the camera control page's
// defaults: its own port, separate from both the streaming listener and the
// operator page, and the same config path reostream itself reads by
// default.
const (
	defaultServeListen = ":8563"
	defaultServeConfig = "/etc/reostream/config.toml"
)

// runServe starts the camera control web page. It never dials a camera:
// it only stands up a listener over the fleet described by the config file,
// and the fleet itself is read fresh on every request.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", defaultServeListen, "HTTP listen address")
	configPath := fs.String("config", defaultServeConfig, "path to reostream's TOML config file")
	allowNoPassword := fs.Bool("allow-no-password", false, "run with no authentication at all")
	fs.Parse(args)

	// This page reaches cameras, the same reason the operator page's
	// control block refuses to start with no password: an unauthenticated
	// listener here is a credential store left open, not a convenience.
	password := os.Getenv("REOCAM_CONTROL_PASSWORD")
	if password == "" && !*allowNoPassword {
		fmt.Fprintln(os.Stderr, "reocam serve: REOCAM_CONTROL_PASSWORD is not set; pass -allow-no-password to run without one")
		os.Exit(2)
	}

	srv, err := camctl.New(camctl.Options{
		Password:        password,
		AllowNoPassword: *allowNoPassword,
		ConfigPath:      *configPath,
		Listen:          *listen,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reocam serve:", err)
		os.Exit(1)
	}

	log.Printf("reocam serve: listening on %s", *listen)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "reocam serve:", err)
		os.Exit(1)
	}
}
