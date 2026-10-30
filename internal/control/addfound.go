package control

import (
	"context"
	"fmt"
	"github.com/VoltTech21/reostream/internal/baichuan"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
)

// Adding several found cameras at once, without leaving the page.
//
// The sweep can find eight cameras in one pass, and the page used to offer
// one "use this one" button per camera that copied its address up into the
// add-one form at the top. Eight cameras meant eight trips up the page, and
// each add posted to /cameras/add, which renders the camera list -- so the
// sweep's results were gone and the next camera meant sweeping again.
//
// This takes the selection in one request and answers with a fragment for
// the same page, so the results stay put and what was added is marked.

// addFoundResult is what the page is told about one attempt.
type addFoundResult struct {
	Added   []string
	Failed  []addFoundFailure
	Nothing bool
}

type addFoundFailure struct {
	Address string
	Reason  string
}

func (s *Server) serveAddFound(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	addrs := r.Form["add"]
	if len(addrs) == 0 {
		s.renderAddFound(w, r, addFoundResult{Nothing: true})
		return
	}

	var out addFoundResult
	for _, key := range addrs {
		// A camera on an NVR or Home Hub is keyed "address#channel": the
		// recorder's address, its login, and the camera's channel.
		addr, channel, _ := strings.Cut(key, "#")
		name := strings.TrimSpace(r.FormValue("name." + key))
		if name == "" {
			name = suggestedName(addr)
			if channel != "" {
				name += "_ch" + channel
			}
		}
		// Credentials per camera, not one pair for the batch: the scan
		// identifies most cameras as admin with no password, but any that
		// needed one carry their own, set on their card when the operator
		// typed it. A per-camera value wins; a batch-wide one (the manual
		// path still posts it) is the fallback; admin is the last resort, so
		// a camera is never written with the empty username that produces a
		// 401 loop.
		user := r.FormValue("username." + key)
		if user == "" {
			user = r.FormValue("username")
		}
		if user == "" {
			user = "admin"
		}
		pass := r.FormValue("password." + key)
		if _, ok := r.Form["password."+key]; !ok {
			pass = r.FormValue("password")
		}
		streams := r.Form["streams."+key]
		if uid := strings.TrimSpace(r.FormValue("uid." + key)); uid != "" {
			// A battery camera: check the login over UDP (one short wake),
			// then add it with no streams so nothing keeps it awake.
			if err := s.checkBatteryLogin(r.Context(), addr, uid, user, pass); err != nil {
				out.Failed = append(out.Failed, addFoundFailure{Address: addr, Reason: err.Error()})
				continue
			}
			if err := s.addOneFound(addr, name, user, pass, nil, uid, ""); err != nil {
				out.Failed = append(out.Failed, addFoundFailure{Address: addr, Reason: err.Error()})
				continue
			}
			out.Added = append(out.Added, fmt.Sprintf("%s as %s", addr, name))
			continue
		}
		if len(streams) == 0 {
			// A camera added with no streams looks like success and does
			// nothing, which is the one mistake this page must not make.
			out.Failed = append(out.Failed, addFoundFailure{
				Address: addr,
				Reason:  "no streams were ticked, so there would be nothing to pull",
			})
			continue
		}
		if err := s.addOneFound(addr, name, user, pass, streams, "", channel); err != nil {
			out.Failed = append(out.Failed, addFoundFailure{Address: addr, Reason: err.Error()})
			continue
		}
		where := addr
		if channel != "" {
			where = fmt.Sprintf("%s channel %s", addr, channel)
		}
		out.Added = append(out.Added, fmt.Sprintf("%s as %s", where, name))
	}
	sort.Strings(out.Added)
	s.renderAddFound(w, r, out)
}

// addOneFound writes one camera into the config, going through exactly the
// same form application and validation that the add-one form does, so a
// camera added from a sweep is held to the same rules as one typed by hand.
// It deliberately does NOT take configMu: writeAndApply takes it itself,
// so locking here deadlocks on the first camera. Adding several is
// therefore a sequence of individual writes rather than one atomic batch,
// which is also what makes a single bad camera fail on its own instead of
// discarding the rest of the selection.
func (s *Server) addOneFound(addr, name, user, pass string, streams []string, uid, channel string) error {
	text, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		return err
	}
	// LoadRaw for the same reason saveCamera uses it: Load would resolve a
	// "$NAME" password reference into the real secret and this writes the
	// result back to disk.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		return err
	}

	form := map[string][]string{
		"name":     {name},
		"address":  {addr},
		"username": {user},
		"password": {pass},
		"streams":  streams,
		"uid":      {uid},
		"channel":  {channel},
	}
	if err := applyCameraForm(cfg, form); err != nil {
		return err
	}
	cam, ok := cameraNamed(cfg, name)
	if !ok {
		return fmt.Errorf("the config produced for %q does not contain it", name)
	}
	if _, err := s.writeAndApply(upsertCameraBlock(text, cam)); err != nil {
		return err
	}
	return nil
}

// checkBatteryLogin logs in to a battery camera over UDP, which wakes it
// for a moment, so a wrong password is caught here rather than later.
func (s *Server) checkBatteryLogin(ctx context.Context, addr, uid, user, pass string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := baichuan.Dial(ctx, addr, baichuan.Options{Username: user, Password: pass, UID: uid})
	if err != nil {
		return fmt.Errorf("could not log in: %s", describeDialError(err))
	}
	defer conn.Close()
	// Over UDP a wrong password is not refused at login; every read after
	// it answers 400 instead (seen on an Argus PT Ultra). So read something.
	_, status, err := baichuan.ReadConfig(ctx, conn, baichuan.ConfigMessages["versioninfo"])
	if err != nil {
		return fmt.Errorf("the camera did not answer: %v", err)
	}
	if status != 200 {
		return fmt.Errorf("wrong password for this camera")
	}
	return nil
}

// suggestedName is a starting point for the name field, not a decision: an
// address is a poor name for a camera but a fine default, and the operator
// edits it in place. "192.0.2.11" becomes "cam-11", which is short, and
// unique within a /24 because the last octet is.
func suggestedName(addr string) string {
	host := addr
	if h, _, ok := strings.Cut(addr, ":"); ok {
		host = h
	}
	if i := strings.LastIndexByte(host, '.'); i >= 0 && i+1 < len(host) {
		return "cam-" + host[i+1:]
	}
	return "cam"
}

// renderAddFound writes the fragment the page swaps in, the same way
// renderDiscover does. NOT s.render: that wraps its output in the site
// layout, which expects a "body" template, so a fragment rendered through
// it comes back empty -- which is exactly what happened here first.
func (s *Server) renderAddFound(w http.ResponseWriter, r *http.Request, res addFoundResult) {
	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t.Funcs(requestFuncs(r))
	if _, err := t.ParseFS(templateFS, "templates/addfound.html"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "addfound", res); err != nil {
		log.Printf("reostream: control: render addfound: %v", err)
	}
}
