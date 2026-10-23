package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
	"github.com/VoltTech21/reostream/internal/onvif"
	"github.com/VoltTech21/reostream/internal/ptz"
	"github.com/VoltTech21/reostream/internal/supervisor"
)

// onvifServers is the set of per-camera ONVIF endpoints, one port each.
type onvifServers struct {
	srvs  []*http.Server
	ctrls []*ptz.Controller
}

// startONVIF opens an ONVIF endpoint for every camera that names one. A
// camera's login is looked up when a session is opened, so a camera edited
// on the control page is reached where it now is.
func startONVIF(cams []config.Camera, sup *supervisor.Supervisor) (*onvifServers, error) {
	o := &onvifServers{}
	for _, cam := range cams {
		if cam.ONVIF == "" {
			continue
		}
		name := cam.Name
		ctrl := ptz.New(ptz.FromBaichuan(func(ctx context.Context) (*baichuan.Conn, error) {
			cfg, ok := sup.CameraConfig(name)
			if !ok {
				return nil, fmt.Errorf("camera %q is no longer configured", name)
			}
			return baichuan.Dial(ctx, cfg.Address, baichuan.Options{
				Username: cfg.Username,
				Password: cfg.Password,
				Channel:  cfg.Channel,
			})
		}), 0, 0)
		ln, err := net.Listen("tcp", cam.ONVIF)
		if err != nil {
			o.Close()
			ctrl.Close()
			return nil, fmt.Errorf("onvif for camera %q: %w", name, err)
		}
		srv := &http.Server{
			Handler:           onvif.NewHandler(ctrl, onvif.Options{Name: name, Username: cam.ONVIFUser, Password: cam.ONVIFPassword}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		o.srvs = append(o.srvs, srv)
		o.ctrls = append(o.ctrls, ctrl)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("reostream: onvif %s: %v", name, err)
			}
		}()
		log.Printf("reostream: onvif for %s listening on %s", name, cam.ONVIF)
	}
	return o, nil
}

// Close stops the endpoints and releases the camera sessions they hold.
func (o *onvifServers) Close() {
	if o == nil {
		return
	}
	for _, s := range o.srvs {
		s.Close()
	}
	for _, c := range o.ctrls {
		c.Close()
	}
}
