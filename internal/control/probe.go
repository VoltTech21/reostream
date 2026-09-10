package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/config"
)

// serveProbe asks a camera what it is and renders the answer as a fragment
// meant to be swapped into the setup form in place, not a full page: it
// deliberately does not go through render and its layout.
func (s *Server) serveProbe(w http.ResponseWriter, r *http.Request) {
	probe := s.opts.Probe
	if probe == nil {
		probe = s.probeGuarded
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

// StreamReport is one stream this page asked a camera about, and what
// asking it established.
//
// There are three outcomes, not two, and conflating any pair of them is
// exactly the kind of misleading symptom this task exists to stop:
//   - present: Codec, Width and Height came back from the camera's own
//     media.
//   - Absent: the camera itself said so. On this path that is only ever
//     Support's noExternStream flag; nothing else here has an equivalent of
//     a config read's 405 for "this model does not have that", because
//     GetStreamInfo learns a stream's shape by opening it and reading real
//     media rather than by asking a config message (see GetStreamInfo's own
//     doc comment on why). Report Absent only where the wire actually says
//     so.
//   - Undetermined: asking failed for some other reason -- a timeout, a
//     held session, a stream this daemon is already using -- and Reason
//     says what. This is not the same as Absent: a working camera whose
//     main stream is merely busy must never be reported the same way as one
//     that genuinely lacks it, or an operator will go reconfigure a camera
//     that was fine.
type StreamReport struct {
	Name   string
	Codec  string
	Width  int
	Height int

	Absent       bool
	Undetermined bool
	Reason       string
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

// probeGuarded is the real Probe: it refuses to dial a camera the daemon
// already holds a live session with, then delegates to probeCamera.
//
// This exists because probeCamera dials up to four real connections to
// whatever address an operator types (see GetStreamInfo's own doc comment
// on why it opens a fresh one per stream), and that address is not
// guaranteed to be unconfigured: an operator re-probing a camera that is
// already in the fleet is an expected use of this page, not a mistake. A
// camera permits one connection per stream, and this daemon's whole
// discipline is that an overlapping connection is the fault to prevent, not
// tolerate and recover from. Checking the daemon's own status before
// dialing is cheap; finding out the hard way, by contending with the
// supervisor's own connection and disturbing a live stream, is not.
func (s *Server) probeGuarded(ctx context.Context, addr, user, pass string) CameraReport {
	if msg, blocked := s.alreadyStreaming(addr); blocked {
		return CameraReport{Err: msg}
	}
	return probeCamera(ctx, addr, user, pass)
}

// alreadyStreaming reports whether addr belongs to a camera this daemon is
// currently streaming, and if so, a message naming what it already knows
// about it, for the setup page to show in place of a fresh probe.
//
// This can only answer as well as the daemon's own wiring allows: it needs
// both a config file to map addr to a camera name and a StatusSource to ask
// whether that camera is live. Either missing means the question genuinely
// cannot be answered here, so this reports "not blocked" rather than
// guessing; see the task-12 report for why that is not the same as the
// guard being pointless; every real deployment (cmd/reostream) wires both.
func (s *Server) alreadyStreaming(addr string) (msg string, blocked bool) {
	if s.opts.ConfigPath == "" || s.opts.Status == nil {
		return "", false
	}
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		return "", false
	}

	want := normalizeAddr(addr)
	var cam *config.Camera
	for i := range cfg.Cameras {
		if normalizeAddr(cfg.Cameras[i].Address) == want {
			cam = &cfg.Cameras[i]
			break
		}
	}
	if cam == nil {
		return "", false
	}

	stats := s.opts.Status.StreamStats()
	var live []string
	for _, stream := range cam.Streams {
		st, ok := stats[cam.Name+"/"+stream]
		if ok && st.Connected {
			live = append(live, fmt.Sprintf("%s (%s)", stream, streamState(st)))
		}
	}
	if len(live) == 0 {
		return "", false
	}
	return fmt.Sprintf(
		"%q at this address is already configured and streaming: %s. Not connecting again: "+
			"this camera allows only one session per stream, and a second connection would "+
			"contend with the one already running.",
		cam.Name, strings.Join(live, ", "),
	), true
}

// normalizeAddr applies the same default-port rule baichuan.Dial does, so a
// config entry written as "192.0.2.50" and an operator typing
// "192.0.2.50:9000" compare equal.
func normalizeAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "9000")
	}
	return addr
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
	anyPresent := false
	for _, name := range []string{"main", "sub", "extern"} {
		if name == "extern" && !sup.HasExternStream() {
			// Support says outright that this model has no balanced
			// stream: the one positive absence signal available on this
			// path. Asking anyway would only wait out a timeout for an
			// answer already known.
			rep.Streams = append(rep.Streams, externAbsentReport())
			continue
		}
		info, err := baichuan.GetStreamInfo(ctx, addr, opts, streamKinds[name])
		sr := streamReport(name, info, err)
		if !sr.Undetermined {
			anyPresent = true
		}
		rep.Streams = append(rep.Streams, sr)
	}
	if !anyPresent {
		rep.Err = "logged in, but no stream on this camera could be confirmed (see detail below)"
	}
	return rep
}

// streamReport turns one GetStreamInfo call's outcome into a StreamReport.
//
// A non-nil err is always Undetermined, never Absent: this path learns a
// stream's shape by opening it and reading real media, which has no
// equivalent of a config read's 405, so nothing here can tell "this model
// does not have this stream" apart from "timed out" or "another connection
// is already using it". Reason keeps the actual cause so a person can judge
// which case they are looking at, rather than this code guessing on their
// behalf and possibly guessing wrong.
func streamReport(name string, info baichuan.StreamInfo, err error) StreamReport {
	if err != nil {
		return StreamReport{Name: name, Undetermined: true, Reason: err.Error()}
	}
	return StreamReport{Name: name, Codec: info.Codec, Width: info.Width, Height: info.Height}
}

// externAbsentReport is the one case on this path with a real, wire-given
// absence signal: Support.noExternStream, established against real cameras
// in internal/baichuan/support.go.
func externAbsentReport() StreamReport {
	return StreamReport{
		Name: "extern", Absent: true,
		Reason: "camera reports no balanced stream (Support.noExternStream)",
	}
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
