package control

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/VoltTech21/reostream/internal/config"
)

// passwordUnchanged is what the form submits when the operator did not
// touch the password field. The page never renders an existing password, so
// it needs a value that means "leave it" and cannot collide with a real
// one; a literal NUL cannot appear in a form field a person typed.
const passwordUnchanged = "\u0000unchanged"

// applyCameraForm folds one camera's form submission into cfg, adding it if
// its name is new and replacing it if not.
// printableField rejects a submitted value that cannot survive the config
// file, and says so in the operator's terms.
//
// Without this, a control character in a name or password reaches the TOML
// encoder, comes back as an escape the parser then refuses, and the page
// reports a TOML error naming a line number in a file the operator never
// opened. The write is correctly refused either way -- saveConfig validates
// through the real loader before anything is written -- so this changes the
// message, not the safety.
//
// Deliberately narrow: only characters that genuinely cannot round-trip are
// refused. A name with spaces, punctuation or non-ASCII letters is fine and
// must stay fine, because camera names on this fleet are things like
// "Sales Floor".
func printableField(label, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("%s cannot contain a line break", label)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s cannot contain control characters", label)
		}
		if r == 0xfffd {
			return fmt.Errorf("%s is not valid text", label)
		}
	}
	return nil
}

func applyCameraForm(cfg *config.Config, form url.Values) error {
	fields := []struct{ label, value string }{
		{"the camera name", form.Get("name")},
		{"the address", form.Get("address")},
		{"the username", form.Get("username")},
	}
	// The password is checked only when it is a real one. The sentinel the
	// form submits for "leave it alone" deliberately begins with a NUL so
	// it cannot collide with anything an operator could type, which is
	// exactly what printableField refuses -- an existing test caught this
	// the first time round.
	if pw := form.Get("password"); pw != passwordUnchanged {
		fields = append(fields, struct{ label, value string }{"the password", pw})
	}
	for _, f := range fields {
		if err := printableField(f.label, f.value); err != nil {
			return err
		}
	}

	cam := config.Camera{
		Name:     form.Get("name"),
		Address:  form.Get("address"),
		Username: form.Get("username"),
		Password: form.Get("password"),
		Streams:  form["streams"],
		RTSP:     form["rtsp"],
	}

	for i, existing := range cfg.Cameras {
		if existing.Name != cam.Name {
			continue
		}
		if cam.Password == passwordUnchanged {
			cam.Password = existing.Password
		}
		cfg.Cameras[i] = cam
		// cfg.Validate: the daemon's own validation runs over the whole
		// config, so a form submission is held to exactly the rules
		// startup enforces.
		return cfg.Validate()
	}

	if cam.Password == passwordUnchanged {
		cam.Password = ""
	}
	cfg.Cameras = append(cfg.Cameras, cam)
	return cfg.Validate()
}

// cameraNamed finds the camera applyCameraForm just folded in, so the
// block written to the file is the validated one rather than a second
// reading of the form.
func cameraNamed(cfg *config.Config, name string) (config.Camera, bool) {
	for _, cam := range cfg.Cameras {
		if cam.Name == name {
			return cam, true
		}
	}
	return config.Camera{}, false
}

// formCamera is a config.Camera with the helpers the template needs.
type formCamera struct {
	config.Camera
}

// HasStream reports whether this camera pulls the named stream, so the
// template can tick the right boxes.
func (c formCamera) HasStream(name string) bool {
	for _, s := range c.Streams {
		if s == name {
			return true
		}
	}
	return false
}

// cameraListEntry is one line of the camera list: a config.toml entry with
// the template's form helpers (formCamera), joined by name with this
// camera's dashboard group, so the same page that edits a camera's config
// also shows whether it is actually streaming and links into its device
// page. A camera cameraGroups has never heard from still gets an entry,
// with a zero-value Group that renders as "no status yet" the same way the
// dashboard does.
type cameraListEntry struct {
	formCamera
	Group cameraGroup
}

type camerasPage struct {
	Title      string
	Cameras    []cameraListEntry
	AllStreams []string
	Unchanged  string
	Error      string
	ReloadNote string
}

func (s *Server) camerasPage() camerasPage {
	page := camerasPage{
		Title:      "Cameras",
		AllStreams: []string{"main", "sub", "extern"},
		Unchanged:  passwordUnchanged,
	}
	// LoadRaw, not Load: this page renders the config's own text back into
	// a form, and Load would resolve any "$NAME" password reference into
	// the real secret before it ever reaches formCamera.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		page.Error = err.Error()
		return page
	}
	for _, cam := range cfg.Cameras {
		page.Cameras = append(page.Cameras, cameraListEntry{
			formCamera: formCamera{Camera: cam},
			Group:      s.cameraGroupFor(cam.Name),
		})
	}
	return page
}

func (s *Server) serveCameras(w http.ResponseWriter, r *http.Request) {
	s.render(w, "cameras.html", s.camerasPage())
}

// cameraBlockText renders one camera as the [[camera]] block a person
// would have typed: lower-case keys, no indentation, in the order the
// Setup page's own example and claimConfigText use.
//
// Password goes through %q like every other value and is NOT resolved on
// the way: the caller hands this a camera read by config.LoadRaw, so a
// password recorded as "$CAM_ONE" is still the literal string "$CAM_ONE"
// here and is written back as one. Resolving it would put an operator's
// real camera credential in config.toml, which is the bug LoadRaw exists
// to prevent.
//
// Optional lists are left out entirely when empty rather than written as
// "[]", because absent is what the loader reads as none and an empty list
// is noise in a file people edit by hand.
//
// %q is not TOML's escaping in every case -- a control character comes out
// as Go's \x00, which TOML does not accept -- but saveConfig validates the
// whole file through the real loader before anything is written, so such a
// value is refused rather than saved as something that will not load.
func cameraBlockText(cam config.Camera) string {
	var b strings.Builder
	b.WriteString("[[camera]]\n")
	fmt.Fprintf(&b, "name = %q\n", cam.Name)
	fmt.Fprintf(&b, "address = %q\n", cam.Address)
	fmt.Fprintf(&b, "username = %q\n", cam.Username)
	fmt.Fprintf(&b, "password = %q\n", cam.Password)
	writeList := func(key string, values []string) {
		if len(values) == 0 {
			return
		}
		quoted := make([]string, len(values))
		for i, v := range values {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		fmt.Fprintf(&b, "%s = [%s]\n", key, strings.Join(quoted, ", "))
	}
	writeList("streams", cam.Streams)
	writeList("rtsp", cam.RTSP)
	return b.String()
}

// findCameraBlock locates the [[camera]] block naming name, as a half-open
// range of line indices into lines, so upsertCameraBlock can replace just
// that block and leave every other byte of the file alone.
//
// It finds table headers by scanning lines rather than by asking the TOML
// decoder, which reports no byte offsets. Two things make that scan safe
// enough to edit a file with: a multi-line string can hold a line that
// looks like a header, so the scan tracks whether it is inside one, and a
// comment can too, so comment lines are skipped. The name itself is read
// by handing the block's own text back to the decoder, so quoting and
// escapes are the loader's business and not a second implementation's.
//
// Trailing blank lines are left OUT of the range: they separate this block
// from the next one, and swallowing them into a replacement would close
// the gap up a little more on every save.
func findCameraBlock(lines []string, name string) (start, end int, found bool) {
	// The ranges of every [[camera]] block, in file order.
	type span struct{ start, end int }
	var spans []span
	var multi string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if multi != "" {
			if strings.Count(line, multi)%2 == 1 {
				multi = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if len(spans) > 0 {
				spans[len(spans)-1].end = i
			}
			if strings.HasPrefix(trimmed, "[[") && strings.TrimSpace(strings.Trim(trimmed, "[]")) == "camera" {
				spans = append(spans, span{start: i, end: len(lines)})
			}
			continue
		}
		for _, delim := range []string{`"""`, `'''`} {
			if strings.Count(line, delim)%2 == 1 {
				multi = delim
				break
			}
		}
	}

	for _, sp := range spans {
		for sp.end > sp.start+1 && strings.TrimSpace(lines[sp.end-1]) == "" {
			sp.end--
		}
		var doc struct {
			Camera []struct{ Name string }
		}
		text := strings.Join(lines[sp.start:sp.end], "\n")
		if _, err := toml.Decode(text, &doc); err != nil || len(doc.Camera) == 0 {
			// A block this cannot read is a block this must not edit.
			// The whole file still has to load for saveConfig to write
			// anything, so an unreadable block here means the save is
			// about to be refused anyway.
			continue
		}
		if doc.Camera[0].Name == name {
			return sp.start, sp.end, true
		}
	}
	return 0, 0, false
}

// upsertCameraBlock returns text with cam's [[camera]] block replaced, or
// appended when there is no block for that name yet.
//
// This is a text edit, not a re-encode. saveCamera used to run the whole
// decoded config back through toml.NewEncoder, which produced a correct
// file that had lost everything about the old one that was not data: the
// comment header a claim writes (including its warning about -listen,
// which a ledger ruling leaned on), the file's own key casing, and the
// order a person had put things in. Adding one camera is one thing
// changed, so one thing is what gets changed.
func upsertCameraBlock(text string, cam config.Camera) string {
	block := cameraBlockText(cam)
	lines := strings.Split(text, "\n")
	start, end, found := findCameraBlock(lines, cam.Name)
	if !found {
		// A blank line before the new block, so the file reads the way
		// the ones people write by hand do. A file that is empty or
		// whitespace-only becomes just the block.
		existing := strings.TrimRight(text, "\n")
		if strings.TrimSpace(existing) == "" {
			return block
		}
		return existing + "\n\n" + block
	}
	var prefix string
	if start > 0 {
		prefix = strings.Join(lines[:start], "\n") + "\n"
	}
	return prefix + block + strings.Join(lines[end:], "\n")
}

// saveCamera folds one camera into the config text and then goes through
// the same write and apply path the raw editor uses. Handing the text to
// writeAndApply is what keeps that one path: a second writer would be a
// second place for the backup, the validation and the reload to drift out
// of agreement.
//
// The config is decoded only to validate the submission against the rules
// startup enforces. What gets written is the file's own text with one
// [[camera]] block changed or added; see upsertCameraBlock.
func (s *Server) saveCamera(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fail := func(err error) {
		page := s.camerasPage()
		page.Error = err.Error()
		s.render(w, "cameras.html", page)
	}

	text, err := loadRawConfig(s.opts.ConfigPath)
	if err != nil {
		fail(err)
		return
	}

	// LoadRaw, not Load: the camera this produces is written straight back
	// into the file below, and Load would turn a "$NAME" password
	// reference into that camera's real, resolved secret first, writing a
	// live credential into config.toml in plaintext. LoadRaw is also what
	// makes "leave the password alone" work: the value the form's sentinel
	// is replaced with is the reference, not the secret behind it.
	cfg, err := config.LoadRaw(s.opts.ConfigPath)
	if err != nil {
		fail(err)
		return
	}

	// Fold the submission into the decoded config purely to validate it:
	// cfg.Validate is the daemon's own check, so the browser is held to
	// exactly the rules startup enforces. cfg itself is not what gets
	// written.
	if err := applyCameraForm(cfg, r.Form); err != nil {
		fail(err)
		return
	}
	cam, ok := cameraNamed(cfg, r.Form.Get("name"))
	if !ok {
		fail(fmt.Errorf("control: the camera form named %q, which is not in the config it just produced", r.Form.Get("name")))
		return
	}

	note, err := s.writeAndApply(upsertCameraBlock(text, cam))
	page := s.camerasPage()
	if err != nil {
		page.Error = err.Error()
	} else {
		page.ReloadNote = note
	}
	s.render(w, "cameras.html", page)
}
