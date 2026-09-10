package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// serveProbe asks a camera what it is and renders the answer as a fragment
// meant to be swapped into the setup form in place, not a full page: it
// deliberately does not go through render and its layout.
func (s *Server) serveProbe(w http.ResponseWriter, r *http.Request) {
	probe := s.opts.Probe
	if probe == nil {
		probe = probeCamera
	}
	rep := probe(r.Context(), r.FormValue("address"), r.FormValue("username"), r.FormValue("password"))

	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := t.ParseFS(templateFS, "templates/probe.html"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "probe", rep); err != nil {
		log.Printf("reostream: control: render probe: %v", err)
	}
}

// StreamReport is one stream a camera says it has.
type StreamReport struct {
	Name   string
	Codec  string
	Width  int
	Height int
}

// CameraReport is what a camera answered when asked what it is. Err is a
// message for a person, not an error value: this is rendered, never
// inspected.
type CameraReport struct {
	Model   string
	Streams []StreamReport
	Err     string
}

// probeTimeout bounds one setup probe. A sweep is not being run here: one
// camera is asked what it is and then, for each stream it plausibly has,
// opened briefly to read its resolution and codec back from its own media
// (see baichuan.GetStreamInfo). That is up to four short connections, not
// one, so this is longer than any single one of them; a first-run user
// staring at a spinner still needs to be told it failed well short of
// reocam's ten minute sweep ceiling.
const probeTimeout = 40 * time.Second

// streamKinds maps the short names this page and its template use onto the
// wire's stream type strings.
var streamKinds = map[string]string{
	"main":   baichuan.StreamMain,
	"sub":    baichuan.StreamSub,
	"extern": baichuan.StreamExtern,
}

// probeCamera asks a camera what it is, for the setup flow.
//
// Failures are translated rather than passed through. A wrong password on
// these cameras produces an empty login reply, which looks identical to a
// camera holding a dead session; reporting that verbatim sends a first time
// user looking at their network instead of at their password.
func probeCamera(ctx context.Context, addr, user, pass string) CameraReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	opts := baichuan.Options{Username: user, Password: pass}
	conn, err := baichuan.Dial(ctx, addr, opts)
	if err != nil {
		return CameraReport{Err: describeDialError(err)}
	}
	defer conn.Close()

	sup, err := baichuan.GetSupport(ctx, conn)
	if err != nil {
		return CameraReport{Err: "connected, but the camera did not answer a support read: " + err.Error()}
	}

	rep := CameraReport{Model: modelName(conn, sup)}
	for _, name := range []string{"main", "sub", "extern"} {
		if name == "extern" && !sup.HasExternStream() {
			// Support says outright that this model has no balanced
			// stream; asking anyway would only wait out a timeout for an
			// answer already known.
			continue
		}
		info, err := baichuan.GetStreamInfo(ctx, addr, opts, streamKinds[name])
		if err != nil {
			// A stream a model does not have is expected to fail here,
			// not to be treated as the probe having gone wrong.
			continue
		}
		rep.Streams = append(rep.Streams, StreamReport{
			Name:   name,
			Codec:  info.Codec,
			Width:  info.Width,
			Height: info.Height,
		})
	}
	if len(rep.Streams) == 0 {
		rep.Err = "logged in, but no stream on this camera answered with any video"
	}
	return rep
}

// modelName reports what a camera calls itself. No marketing model name
// ("RLC-810A" and similar) appears anywhere on the wire in this protocol:
// not in Support, and not in the DeviceInfo document a camera sends as its
// login reply (see internal/baichuan/deviceinfo.go). DeviceInfo's own device
// type and top resolution are real data, already in hand from login at no
// extra round trip, so they are what is reported rather than inventing a
// name the camera never sent.
func modelName(conn *baichuan.Conn, sup baichuan.Support) string {
	kind := "camera"
	res := ""
	if di, err := baichuan.ParseDeviceInfo(conn.DeviceInfo()); err == nil {
		if di.DeviceInfo.TypeInfo != "" {
			kind = di.DeviceInfo.TypeInfo
		}
		res = di.DeviceInfo.Resolution.Name
	}
	channels := sup.Support.ChannelNum
	switch {
	case res != "" && channels > 1:
		return fmt.Sprintf("%s, %s, %d channels", kind, res, channels)
	case res != "":
		return fmt.Sprintf("%s, %s", kind, res)
	default:
		return kind
	}
}

// describeDialError turns a connection failure into something a first time
// user can act on. The 401 case is the one that matters most: it is the
// single most likely thing to be wrong on a first attempt, and its raw
// symptom, an empty login reply, names neither authentication nor the
// password.
func describeDialError(err error) string {
	switch {
	case errors.Is(err, baichuan.ErrUnauthorised):
		return "authentication failed (401): check the username and password"
	case strings.Contains(err.Error(), "connection refused"):
		return "nothing is listening on port 9000 at that address"
	case errors.Is(err, context.DeadlineExceeded):
		return "the camera did not answer in time: check the address, and that nothing else is holding its session"
	default:
		return err.Error()
	}
}
