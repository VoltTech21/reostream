package control

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Auto tracking's horizontal range (track limit, 434/435): the pan positions
// the camera will not track past, -1 for no limit. The Reolink app sets each
// limit by turning the camera there and taking its position (ptz cur pos,
// 433); it also uploads a picture of each end for its own display, which
// the camera does not need, so it is not done here (captured 2026-10-26).

type trackLimitView struct {
	Left, Right int
	Pan         int
	HavePan     bool
}

func (v trackLimitView) Label(n int) string {
	if n < 0 {
		return "no limit"
	}
	return "pan position " + strconv.Itoa(n)
}

func intField(doc []byte, path string) (int, bool) {
	v, ok := fieldValue(doc, path)
	n, err := strconv.Atoi(strings.TrimSpace(v))
	return n, ok && err == nil
}

func (s *Server) readTrackLimit(ctx context.Context, conn *baichuan.Conn, page *cameraPage) {
	pair, ok := pairForName("get ai track limit cfg")
	if !ok {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(rctx, conn, pair.Get)
	cancel()
	if err != nil || status != 200 {
		return
	}
	v := &trackLimitView{}
	v.Left, _ = intField(doc, "trackLimit/leftLimit")
	v.Right, _ = intField(doc, "trackLimit/rightLimit")
	rctx, cancel = context.WithTimeout(ctx, readTimeout)
	pos, st, err := baichuan.ReadConfig(rctx, conn, 433)
	cancel()
	if err == nil && st == 200 {
		v.Pan, v.HavePan = intField(pos, "ptzCurPos/pPos")
	}
	page.TrackLimit = v
}

// serveTrackLimit sets one end of the range to where the camera points now,
// or clears both.
func (s *Server) serveTrackLimit(w http.ResponseWriter, r *http.Request) {
	cam, err := s.byName(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	side := r.FormValue("side")
	back := returnTo(r, "/cameras/"+url.PathEscape(cam.Name))
	subject := cam.Name + ": tracking range"
	refuse := func(why string) {
		s.setFlash(w, r, flashFor(subject, WriteResult{Outcome: "refused", Detail: why}))
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	if side != "left" && side != "right" && side != "full" {
		http.Error(w, "side is left, right or full", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	conn, err := s.dial(ctx, cam)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pair, _ := pairForName("get ai track limit cfg")
	rctx, rcancel := context.WithTimeout(ctx, readTimeout)
	doc, status, err := baichuan.ReadConfig(rctx, conn, pair.Get)
	rcancel()
	pan := -1
	if side != "full" {
		rctx, rcancel := context.WithTimeout(ctx, readTimeout)
		pos, st, perr := baichuan.ReadConfig(rctx, conn, 433)
		rcancel()
		p, ok := intField(pos, "ptzCurPos/pPos")
		if perr != nil || st != 200 || !ok {
			conn.Close()
			refuse("could not read where the camera points")
			return
		}
		// -1 is the camera not knowing, and writing it as an edge means no
		// limit, which would clear the setting while the page says saved.
		if p < 0 {
			conn.Close()
			refuse("the camera does not know where it points; calibrate it first")
			return
		}
		pan = p
	}
	conn.Close()
	if err != nil || status != 200 {
		refuse(fmt.Sprintf("this camera has no tracking range (%v, status %d)", err, status))
		return
	}
	set := func(path, v string) {
		if err == nil {
			doc, err = setField(doc, path, v)
		}
	}
	switch side {
	case "left":
		set("trackLimit/leftLimit", strconv.Itoa(pan))
	case "right":
		set("trackLimit/rightLimit", strconv.Itoa(pan))
	default:
		set("trackLimit/leftLimit", "-1")
		set("trackLimit/rightLimit", "-1")
		set("trackLimit/leftImageName", "")
		set("trackLimit/rightImageName", "")
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	result, werr := s.writeBlock(ctx, cam, pair.Set, doc, true)
	if werr != nil {
		http.Error(w, werr.Error(), http.StatusBadGateway)
		return
	}
	s.setFlash(w, r, flashFor(subject, result))
	http.Redirect(w, r, back, http.StatusSeeOther)
}
