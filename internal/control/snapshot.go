package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// A still from the camera itself
//
// GET /cameras/{name}/snapshot.jpg is a picture the camera makes, asked for
// with Baichuan's own snap message (109) on a connection of its own. It is
// not the page's camera button, which draws whatever frame the browser's
// player is showing onto a canvas and so only exists while a tile is
// playing, and it is not a battery camera's last picture, which is the
// keyframe kept from whenever it was last awake. Someone scripting a
// picture of a battery camera (#28, an Argus that "if it hasn't already
// been woken up, the image I grab will be old") wants neither: they want
// the camera, now.
//
// That is what the dial gives a battery camera. Every session over its UDP
// transport wakes it, so asking for a still is enough to get a fresh one,
// and closing the connection afterwards is what lets it sleep again. The
// dial is marked as this process's own (bcudp.Ours), so the camera waking
// for it is not reported as motion.
//
// The camera encodes the still, so this needs no decoder: the bytes are the
// camera's JPEG, passed through. Measured 2026-11-03 against a TrackMix
// (wired) and an Argus (battery, woken for it); see docs/control.md.

// snapWait bounds the wait for the picture once the camera has accepted
// the connection. Measured stills arrive in well under a second on a wired
// camera; a battery camera that has just woken takes longer.
var snapWait = 20 * time.Second

// snapLocks keeps one snap per camera in flight. A script polling faster
// than the camera answers would otherwise stack connections on it, and on
// a battery camera every one of those is battery.
var snapLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func snapLock(camera string) *sync.Mutex {
	snapLocks.mu.Lock()
	defer snapLocks.mu.Unlock()
	if snapLocks.m == nil {
		snapLocks.m = map[string]*sync.Mutex{}
	}
	l := snapLocks.m[camera]
	if l == nil {
		l = &sync.Mutex{}
		snapLocks.m[camera] = l
	}
	return l
}

// jpegMagic is the start of image marker every JPEG begins with.
var jpegMagic = []byte{0xff, 0xd8, 0xff}

func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The snap message picks the encoder by name and the camera knows
	// two: main is the full resolution, sub the small one. Anything else
	// is refused here rather than sent to find out what the camera does
	// with it.
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "main"
	}
	if stream != "main" && stream != "sub" {
		http.Error(w, fmt.Sprintf("stream must be main or sub, not %q", stream), http.StatusBadRequest)
		return
	}

	lock := snapLock(cam.Name)
	lock.Lock()
	defer lock.Unlock()
	if r.Context().Err() != nil {
		// Gave up while another snap of this camera was in flight.
		return
	}

	start := time.Now()
	// No deadline here, so s.dial applies its own: a wired camera answers a
	// login in well under a second, a battery camera may first have to
	// wake, and that function already knows the difference.
	conn, err := s.dial(r.Context(), cam)
	if err != nil {
		code, msg := snapDialError(cam, err)
		log.Printf("control: %s: snapshot: %v", cam.Name, err)
		if code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", strconv.Itoa(int(silentFor.Seconds())))
		}
		http.Error(w, msg, code)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(r.Context(), snapWait)
	defer cancel()
	img, file, err := baichuan.TakeSnap(ctx, conn, stream)
	if err != nil {
		code, msg := snapError(cam, stream, err)
		log.Printf("control: %s: snapshot %s: %v", cam.Name, stream, err)
		http.Error(w, msg, code)
		return
	}
	if !bytes.HasPrefix(img, jpegMagic) {
		// Never seen, and not guessed at either: labelling it a JPEG would
		// hand a script a file that fails somewhere further away.
		log.Printf("control: %s: snapshot %s: %d bytes that are not a JPEG (%s)", cam.Name, stream, len(img), file)
		http.Error(w, fmt.Sprintf("%s sent %d bytes for its %s still that are not a JPEG", cam.Name, len(img), stream), http.StatusBadGateway)
		return
	}
	took := time.Since(start).Round(10 * time.Millisecond)
	log.Printf("control: %s: snapshot %s, %d bytes in %v (%s)", cam.Name, stream, len(img), took, file)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	// Every request is a new picture; nothing in between may keep one.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", cam.Name+"_"+stream+".jpg"))
	w.Write(img)
}

// snapDialError turns a failed dial into a status and a sentence. A
// timeout is the camera not answering (504); a camera that timed out
// moments ago is not asked again yet (503); anything else is the camera
// or the network refusing (502).
func snapDialError(cam Camera, err error) (int, string) {
	switch {
	case errors.Is(err, errCameraSilent):
		return http.StatusServiceUnavailable, fmt.Sprintf("%s: %v", cam.Name, err)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		if cam.UID != "" {
			return http.StatusGatewayTimeout, fmt.Sprintf("%s did not wake: no answer within %v. A battery camera out of Wi-Fi range or with a flat battery looks like this.", cam.Name, pageDialTimeoutBattery)
		}
		return http.StatusGatewayTimeout, fmt.Sprintf("%s did not answer within %v", cam.Name, pageDialTimeout)
	}
	return http.StatusBadGateway, fmt.Sprintf("could not connect to %s: %v", cam.Name, err)
}

// snapError is snapDialError for the snap itself, on a connection that
// logged in.
func snapError(cam Camera, stream string, err error) (int, string) {
	var refused *baichuan.SnapRefusedError
	switch {
	case errors.As(err, &refused):
		return http.StatusBadGateway, fmt.Sprintf("%s refused the %s still: %s", cam.Name, stream, baichuan.ExplainStatus(refused.Status))
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, fmt.Sprintf("%s logged in but sent no %s still within %v", cam.Name, stream, snapWait)
	}
	return http.StatusBadGateway, fmt.Sprintf("%s: %v", cam.Name, err)
}
