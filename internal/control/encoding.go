package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// encodingConfirmReason is shown before a write, for the same reason the
// floodlight and the fisheye mode ask: this one changes what the camera
// records, not a document.
const encodingConfirmReason = "This restarts the stream for a few seconds. Change the encoding?"

// encodingPage is the encoder surface for one camera.
//
// Options carries EVERY combination the camera offers rather than a merged
// list of resolutions and bitrates, because the two are not independent:
// the pano offers 12288 kbps at 7680x2160 and refuses it at 4096x1152. The
// form narrows the choices as the resolution changes; the server checks the
// result against the same table, because the form is only a convenience and
// a POST can say anything.
type encodingPage struct {
	Current cgi.Enc
	Options []cgi.EncOptions
	Err     string

	// Have says the settings came from the camera. Same discipline as the
	// aligner: no controls over a reading that failed, because a zero Enc
	// is a valid-looking set of zeros and this form writes.
	Have bool

	Confirm string
}

// OptionsJSON is the whole options table, for the form's own use.
//
// The resolution control has to rewrite the bitrate and frame-rate stops
// beside it when it changes, and it cannot do that from the rendered
// options alone -- those are only the stops for the size in force. So the
// table travels with the page. It is read back out of a data attribute, so
// a page with no JavaScript simply keeps the stops it was rendered with,
// and the server checks the result either way.
func (p encodingPage) OptionsJSON() string {
	type stream struct {
		Size       string `json:"size"`
		BitRates   []int  `json:"bitRates"`
		FrameRates []int  `json:"frameRates"`
		GOPMin     int    `json:"gopMin"`
		GOPMax     int    `json:"gopMax"`
	}
	type combo struct {
		Main stream `json:"main"`
		Sub  stream `json:"sub"`
	}
	out := make([]combo, 0, len(p.Options))
	for _, o := range p.Options {
		out = append(out, combo{
			Main: stream{o.Main.Size, o.Main.BitRates, o.Main.FrameRates, o.Main.GOPMin, o.Main.GOPMax},
			Sub:  stream{o.Sub.Size, o.Sub.BitRates, o.Sub.FrameRates, o.Sub.GOPMin, o.Sub.GOPMax},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// MainOptionsFor returns the options belonging to a resolution, which is
// what the template needs to render the stops for the size in force.
func (p encodingPage) MainOptionsFor(size string) cgi.EncStreamOptions {
	for _, o := range p.Options {
		if o.Main.Size == size {
			return o.Main
		}
	}
	if len(p.Options) > 0 {
		return p.Options[0].Main
	}
	return cgi.EncStreamOptions{}
}

// SubOptionsFor is the same for the sub stream.
func (p encodingPage) SubOptionsFor(size string) cgi.EncStreamOptions {
	for _, o := range p.Options {
		if o.Sub.Size == size {
			return o.Sub
		}
	}
	if len(p.Options) > 0 {
		return p.Options[0].Sub
	}
	return cgi.EncStreamOptions{}
}

// MainSizes lists the resolutions the main stream offers, in the camera's
// own order.
func (p encodingPage) MainSizes() []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range p.Options {
		if o.Main.Size != "" && !seen[o.Main.Size] {
			seen[o.Main.Size] = true
			out = append(out, o.Main.Size)
		}
	}
	return out
}

// readEncoding fills the page from the camera.
func readEncoding(ctx context.Context, c *cgi.Client, channel int) (encodingPage, error) {
	cur, opts, err := c.GetEnc(ctx, channel)
	if err != nil {
		return encodingPage{}, err
	}
	return encodingPage{
		Current: cur,
		Options: opts,
		Have:    len(opts) > 0,
		Confirm: encodingConfirmReason,
	}, nil
}

// checkEncChanges checks only the streams a save changed. A camera behind
// an NVR can run a bitrate the NVR does not list (cam-19: 6144 kbps at
// 3840*2160, with 4096 the highest offered), and turning the microphone on
// must not be refused over a setting nobody touched.
func checkEncChanges(cur, next cgi.Enc, page encodingPage) error {
	if next.Main != cur.Main {
		if err := checkEncChoice(next.Main, page.MainOptionsFor(next.Main.Size), "main"); err != nil {
			return err
		}
	}
	if next.Sub != cur.Sub {
		if err := checkEncChoice(next.Sub, page.SubOptionsFor(next.Sub.Size), "sub"); err != nil {
			return err
		}
	}
	return nil
}

// checkEncChoice reports whether one stream's requested settings are a
// combination the camera said it would take.
//
// Checked here and not only in the form, because the form narrows choices
// with JavaScript and a POST is not obliged to have run any. The camera
// would refuse an impossible pair anyway; the difference is that this
// refusal can say which field was wrong and what was on offer, where the
// camera's is a status code.
func checkEncChoice(want cgi.EncStream, opts cgi.EncStreamOptions, stream string) error {
	if opts.Size == "" {
		return fmt.Errorf("this camera offered no options for the %s stream, so nothing here can be checked against it", stream)
	}
	if want.Size != opts.Size {
		return fmt.Errorf("the %s stream does not offer %s", stream, want.Size)
	}
	if !containsInt(opts.BitRates, want.BitRate) {
		return fmt.Errorf("%s at %s does not take %d kbps. It offers %s",
			stream, opts.Size, want.BitRate, joinInts(opts.BitRates))
	}
	if !containsInt(opts.FrameRates, want.FrameRate) {
		return fmt.Errorf("%s at %s does not take %d fps. It offers %s",
			stream, opts.Size, want.FrameRate, joinInts(opts.FrameRates))
	}
	if want.GOP < opts.GOPMin || want.GOP > opts.GOPMax {
		return fmt.Errorf("%s interframe space must be between %d and %d, not %d",
			stream, opts.GOPMin, opts.GOPMax, want.GOP)
	}
	if len(opts.Profiles) > 0 && !containsString(opts.Profiles, want.Profile) {
		return fmt.Errorf("%s does not take the %q profile", stream, want.Profile)
	}
	return nil
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func joinInts(xs []int) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += strconv.Itoa(x)
	}
	if out == "" {
		return "nothing"
	}
	return out
}

// serveApplyEncoding writes the encoder settings.
func (s *Server) serveApplyEncoding(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	c, err := s.cgiDial(cam)
	if err != nil {
		s.encodingOutcome(w, r, name, "refused", fmt.Sprintf("could not reach the camera: %v", err))
		return
	}
	defer c.Close()

	// Read first. The write sends the whole block, so the fields this form
	// does not touch -- audio, and the sub stream when only main changed --
	// have to come from the camera rather than from zero values.
	cur, opts, err := c.GetEnc(ctx, cam.Channel)
	if err != nil {
		s.encodingOutcome(w, r, name, "refused", fmt.Sprintf("could not read the current encoding: %v", err))
		return
	}
	page := encodingPage{Current: cur, Options: opts, Have: len(opts) > 0}

	next := cur
	if sz := r.FormValue("main_size"); sz != "" {
		next.Main.Size = sz
		// The numbers travel with the string: a size that disagrees with
		// its own width and height is a value no camera was asked to read.
		o := page.MainOptionsFor(sz)
		next.Main.Width, next.Main.Height = o.Width, o.Height
	}
	if err := applyIntField(r, "main_bitrate", &next.Main.BitRate); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}
	if err := applyIntField(r, "main_framerate", &next.Main.FrameRate); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}
	if err := applyIntField(r, "main_gop", &next.Main.GOP); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}
	if err := applyIntField(r, "sub_bitrate", &next.Sub.BitRate); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}
	if err := applyIntField(r, "sub_framerate", &next.Sub.FrameRate); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}
	if err := applyIntField(r, "sub_gop", &next.Sub.GOP); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}

	// The camera's microphone: Enc.audio. Off sends silent audio frames
	// (measured -91 dB on a TrackMix with it at 0), on sends the room.
	if v := r.FormValue("audio"); v != "" {
		next.Audio = 0
		if v == "1" {
			next.Audio = 1
		}
	}

	if err := checkEncChanges(cur, next, page); err != nil {
		s.encodingOutcome(w, r, name, "refused", err.Error())
		return
	}

	if next == cur {
		s.encodingOutcome(w, r, name, "confirmed", "was already set to that, so nothing was written")
		return
	}

	if err := c.SetEnc(ctx, cam.Channel, next); err != nil {
		s.encodingOutcome(w, r, name, "refused", fmt.Sprintf("the camera did not take it: %v", err))
		return
	}
	if next.Audio != cur.Audio {
		after, _, err := c.GetEnc(ctx, cam.Channel)
		if err != nil || after.Audio != next.Audio {
			s.encodingOutcome(w, r, name, "refused", "audio was accepted but the camera still reports it otherwise")
			return
		}
		if next.Main == cur.Main && next.Sub == cur.Sub && next.Extern == cur.Extern {
			state := "off"
			if next.Audio == 1 {
				state = "on"
			}
			s.encodingOutcome(w, r, name, "confirmed", "audio "+state)
			return
		}
	}
	s.encodingOutcome(w, r, name, "confirmed",
		"saved: "+encodingChanged(cur, next)+". Readers of that stream reconnect within a few seconds.")
}

// encodingChanged describes the streams this write actually moved.
//
// It used to describe main unconditionally, which reads as a lie the first
// time somebody changes only the sub stream: the page says "main is now
// 4096*1152 at 10240 kbps" about a main stream nobody touched, and the one
// setting that did change is not mentioned anywhere. Measured against the
// pano, which is how it was found.
//
// The caller only reaches here when something changed, so an empty result
// is not possible; it returns a sentence that admits that rather than an
// empty string, because a message ending " . Readers of" would be a worse
// way to find out.
func encodingChanged(cur, next cgi.Enc) string {
	var parts []string
	if next.Main != cur.Main {
		parts = append(parts, describeEncStream("main", next.Main))
	}
	if next.Sub != cur.Sub {
		parts = append(parts, describeEncStream("sub", next.Sub))
	}
	if len(parts) == 0 {
		return "nothing changed"
	}
	return strings.Join(parts, "; ")
}

func describeEncStream(stream string, s cgi.EncStream) string {
	return fmt.Sprintf("%s is now %s at %d kbps, %d fps, interframe space %d",
		stream, s.Size, s.BitRate, s.FrameRate, s.GOP)
}

// applyIntField reads one optional integer form value into dst.
func applyIntField(r *http.Request, field string, dst *int) error {
	raw := r.FormValue(field)
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%s must be a whole number, not %q", field, raw)
	}
	*dst = n
	return nil
}

// encodingOutcome reports the result the way every other camera write on
// this page does: a flash, then back to the camera page.
//
// A success here carries its detail, which flashFor deliberately drops.
// That rule is right for the writes it was written for -- "lounge: camera
// name saved" needs no paragraph -- and wrong for this one in both
// directions. A write that landed should say what the camera is recording
// at now, because the whole point was changing it; and a submission that
// changed nothing must NOT say "saved", which is what it said before this,
// claiming a write that never happened.
func (s *Server) encodingOutcome(w http.ResponseWriter, r *http.Request, camera, outcome, detail string) {
	if outcome == "confirmed" && detail != "" {
		s.setFlash(w, r, Flash{Outcome: outcome, Message: camera + ": encoding " + detail})
	} else {
		s.setFlash(w, r, flashFor(camera+": encoding", WriteResult{
			Outcome: outcome,
			Detail:  detail,
		}))
	}
	http.Redirect(w, r, "/cameras/"+url.PathEscape(camera)+"/settings/streams", http.StatusSeeOther)
}
