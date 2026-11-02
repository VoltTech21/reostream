package control

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/VoltTech21/reostream/internal/config"
)

// TalkGains are the choices the Audio page offers for talk_gain_db.
func (cameraPage) TalkGains() []int { return []int{-12, -9, -6, -3, 0, 3, 6, 9, 12} }

// serveTalkGain saves the camera's talk_gain_db: how much reostream turns
// talk up or down before it reaches the camera's speaker. It is
// reostream's own setting, in the config file, not the camera's, and
// changing it restarts no stream; the next talk session uses it.
func (s *Server) serveTalkGain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	back := "/cameras/" + url.PathEscape(name) + "/settings/audio"
	flash := func(outcome, msg string) {
		s.setFlash(w, r, Flash{Outcome: outcome, Message: name + ": " + msg})
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	db, err := strconv.Atoi(r.FormValue("db"))
	if err != nil || db < -12 || db > 12 || db%3 != 0 {
		flash("refused", fmt.Sprintf("talk gain %q is not -12 to 12 dB in steps of 3", r.FormValue("db")))
		return
	}
	text, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		flash("refused", "talk gain: "+err.Error())
		return
	}
	// LoadRaw, as saveCamera: the block is written back, and a "$NAME"
	// password must stay a reference.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		flash("refused", "talk gain: "+err.Error())
		return
	}
	cam, ok := cameraNamed(cfg, name)
	if !ok {
		http.Error(w, "no camera named "+name, http.StatusNotFound)
		return
	}
	cam.TalkGainDB = db
	if _, err := s.writeAndApply(upsertCameraBlock(text, cam)); err != nil {
		flash("refused", "talk gain: "+err.Error())
		return
	}
	if db == 0 {
		flash("confirmed", "talk gain 0 dB")
	} else {
		flash("confirmed", fmt.Sprintf("talk gain %+d dB", db))
	}
}
