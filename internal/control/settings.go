// Curated settings: a small number of fields a person actually changes,
// picked out of the raw blocks blocks.html already exposes wholesale, so
// changing the camera's name or its timestamp overlay does not require
// editing an XML document by hand.
package control

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Field is one curated control: a human label, the path to the element it
// edits, and how to present it.
//
// XPath is a "/"-separated chain of element names, read from a document's
// root and matched wherever that exact chain occurs, so a caller never has
// to spell out the wrapping <body> (or a channel container) a read happens
// to come back inside. "Osd/osdChannel/name" is the whole address; it does
// not change if the camera wraps its reply differently than expected.
//
// Kind is never a Go type. "warning" fields carry no XPath at all: they
// exist only to put text next to a control, which is how the
// UnsafeToRewrite label surfaces here without hiding the
// control it warns about.
type Field struct {
	Label string
	XPath string
	Kind  string

	// YPath is the second element a control that edits a PAIR of elements
	// changes alongside XPath. It is empty for every ordinary field, which
	// edits exactly one element.
	//
	// It exists for Kind "position": an overlay's corner is not one value
	// on these cameras, it is topLeftX and topLeftY, and either one on its
	// own is a corner nobody chose. XPath addresses the X, YPath the Y,
	// and serveApplySetting applies both inside a single read-modify-write
	// -- see parseEdits.
	YPath string

	// Slug is a stable id for the field's row. Governs, on a toggle, is
	// the Slug of the row that toggle switches on and off.
	Slug    string
	Governs string

	// NoCopy keeps a field out of "Copy to other cameras": a camera's own
	// name is not something to give every other camera.
	NoCopy bool

	// Help is the one line behind the field's "i". Empty for a field whose
	// label says everything.
	Help string

	// Invert, when non-zero, is a number field the camera stores upside
	// down: the page shows Invert minus the stored value and writes Invert
	// minus what is entered, matching the Reolink app.
	Invert int

	// Min and Max are a number field's range as the page shows it (after
	// Invert), when it is known. The slider is held to it and a value
	// outside it is refused before anything is sent: the camera does not
	// say its ranges, and it took a motion sensitivity of -16 and a volume
	// of 116 when the slider ran 0 to 255 for everything (click test,
	// 10-27). Max 0 means the range is not known.
	Min, Max int

	// Also lists further elements a toggle sets to the same value in the
	// same write: record audio is one switch, but the main and the sub
	// stream each carry their own <audio>.
	Also []string

	// Variant is the "#..." part of the group's block, stamped by groups():
	// the same paths in different documents (person, vehicle and animal
	// detection) keep their values apart on the page. See Key.
	Variant string

	// Choices, on a Kind "choice" field, are the values offered as stops.
	// Only the value the camera was seen holding is observed; the rest are
	// the firmware's documented spellings. The camera's own value is always
	// offered too, so a camera holding something unlisted keeps it.
	Choices []Choice
}

// Choice is one stop on a Kind "choice" field.
type Choice struct {
	Value string
	Label string
}

// Paths lists every element this field edits, primary first: one path for
// an ordinary field, two for a position. A "warning" field, which carries
// no XPath at all, edits nothing and lists nothing.
func (f Field) Paths() []string {
	if f.XPath == "" {
		return nil
	}
	out := []string{f.XPath}
	if f.YPath != "" {
		out = append(out, f.YPath)
	}
	return append(out, f.Also...)
}

// shown is the value the page presents for a stored one.
func (f Field) shown(stored string) string {
	if f.Invert == 0 {
		return stored
	}
	n, err := strconv.Atoi(strings.TrimSpace(stored))
	if err != nil {
		return stored
	}
	return strconv.Itoa(f.Invert - n)
}

// stored is the value written for one entered on the page.
func (f Field) stored(entered string) (string, error) {
	if f.Kind == "number" && f.Max > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(entered))
		if err != nil {
			return "", fmt.Errorf("%s takes a whole number, not %q", f.Label, entered)
		}
		if n < f.Min || n > f.Max {
			return "", fmt.Errorf("%s goes from %d to %d, not %d", f.Label, f.Min, f.Max, n)
		}
	}
	if f.Invert == 0 {
		return entered, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(entered))
	if err != nil {
		return "", fmt.Errorf("%s takes a whole number, not %q", f.Label, entered)
	}
	return strconv.Itoa(f.Invert - n), nil
}

// Key is where the page keeps the value read from path: the path itself,
// or the variant and the path for a field in a variant block.
func (f Field) Key(path string) string {
	if f.Variant == "" {
		return path
	}
	return f.Variant + ":" + path
}

// FormXPath is what the settings form posts as this field's xpath: the one
// element it edits, or the comma-separated list of elements it changes
// together. A single-element field posts exactly the string it always did,
// so nothing about an existing form's wire format changes.
func (f Field) FormXPath() string {
	if f.YPath == "" {
		return f.XPath
	}
	return f.XPath + "," + f.YPath
}

// Group is one curated section of the settings page, every field editing
// the same raw block.
//
// Blocks lists candidate blocks, by name exactly as baichuan.ConfigPair.Name
// gives it ("osd get", "get osd"), most preferred first. More than one
// candidate exists only for OSD: docs/control.md records "osd get" (44) and
// "get osd" (29) as two independently recovered pairs for the same
// settings, and normalizing them onto one name is what pairs a write to the
// wrong message id (see ConfigPairs' own comment). Which one a given camera
// actually implements is not assumed; resolveGroupBlock discovers it by
// trying each candidate and taking the first that reads back 200.
//
// A defaults-only read such as "osd def get" (110) can never end up in
// Blocks by accident and do damage if it did: ConfigPairs only pairs a
// "X get" with a "X set" that shares its exact name, and no "osd def set"
// exists, so pairForName can never resolve a defaults read to a writable
// pair at all. That is what makes it safe for this list to be no more
// careful than "every writable candidate for this setting, most likely
// first": the factory-defaults trap is closed by construction, not by
// caution here.
type Group struct {
	Title  string
	Blocks []string
	Fields []Field
	// ConfirmReason, when non-empty, is shown next to the group's heading
	// and used as the confirmation prompt every one of its forms asks
	// through before it submits. Empty for a group like Picture and OSD,
	// where nothing switches anything a person or a camera can see happen;
	// set for Lights and IR, where every field is an emitter.
	ConfirmReason string
	// InterruptsStream tags the heading of a group whose write makes the
	// camera rebuild its pipeline. Blurb is the one plain line under it.
	InterruptsStream bool
	Blurb            string
	// Reboots marks a group whose every write restarts the camera (the
	// encoder blocks do this). It is the highest of the three save tiers.
	Reboots bool
	// Optional hides the whole group on a camera that does not answer its
	// block, rather than listing every field as not available: a spotlight
	// or AI noise reduction is something a model has or has not.
	Optional bool
	// NeedsFloodlight hides the group on a camera whose Support block says
	// it has no light. Answering the block is no proof: an RLC-810A, which
	// has no spotlight, answers floodlight task get with a full document.
	NeedsFloodlight bool
	// NeedsFTP hides the group on a camera whose Support block says it
	// has no FTP (Support.LacksFTP). Those builds answer every FTP request
	// with 400, which the page would otherwise show as a refused read.
	NeedsFTP bool
	// Untested marks a group whose fields were read off a real camera but
	// whose writes nobody has yet watched take effect on one. It shows as
	// the hollow dot, the redesign's fourth tier.
	Untested bool
}

// Tier is the save tier shown as a colored dot before every setting in the
// group, per the redesign: instant (nothing stops), interruption (the
// stream rebuilds briefly), reboot (the camera restarts). There is no
// "untested" tier here -- every curated setting has been exercised on a real
// camera; untested is reserved for the few things that genuinely are not
// (PTZ auto-tracking), which live outside these groups.
func (g Group) Tier() string {
	switch {
	case g.Untested:
		return "untested"
	case g.Reboots:
		return "reboot"
	case g.InterruptsStream || g.ConfirmReason != "":
		return "interruption"
	default:
		return "instant"
	}
}

// TierLabel is the plain sentence behind the dot, for its title/popover.
func (g Group) TierLabel() string {
	switch g.Tier() {
	case "reboot":
		return "Saving this restarts the camera"
	case "untested":
		return "Not yet verified on a camera"
	case "interruption":
		return "Saving this briefly interrupts the stream"
	default:
		return "Saves instantly, nothing stops"
	}
}

// The overlay corner table, and how it was established.
//
// Each overlay in the "osd get" block (message 44) carries topLeftX and
// topLeftY next to its enable flag:
//
//	<OsdDatetime>    <enable>1</enable> <topLeftX>1</topLeftX>     <topLeftY>1</topLeftY>     ...
//	<OsdChannelName> <enable>0</enable> <topLeftX>65536</topLeftX> <topLeftY>65536</topLeftY> ...
//
// These are NOT pixel coordinates, despite the names. Read across four
// live cameras on one fleet, the two fields only ever held 1 or 65536. One
// camera had its timestamp at topLeftX=65536, topLeftY=1 while the other
// three had 1,1; pulling a frame off each stream and looking at it settled
// what that meant: that camera's clock is drawn in the TOP RIGHT and the
// others' in the top left. Every camera name on that fleet sat at
// 65536,65536, and every camera name was drawn bottom right. So the pair
// is a corner encoding, 1 meaning "against this edge" and 65536 "against
// the far edge":
//
//	corner        topLeftX  topLeftY
//	top left      1         1
//	top right     65536     1
//	bottom left   1         65536
//	bottom right  65536     65536
//
// This came from hardware, not from documentation: no Reolink document
// this project has seen says any of it, and it is written down here so
// nobody has to pull frames off four cameras a second time to recover it.
//
// The honest limit: only those two values were ever OBSERVED. Whether a
// camera would accept, say, 32768 and center an overlay is simply not
// known, and this control does not pretend to know by offering it. An
// operator who wants to find out can still write topLeftX and topLeftY to
// anything from the advanced page, where every field of the raw block
// stays editable; and a camera already reporting a pair this table has no
// name for keeps it, shown as itself, rather than being snapped to the
// nearest corner (see cameraPage.Position).
const (
	osdNearEdge = "1"
	osdFarEdge  = "65536"
	osdCenter   = "65537"
)

// osdCorner is one row of the table above: the wording a person picks and
// the pair of values picking it writes.
type osdCorner struct {
	Label string
	X, Y  string
}

// osdCorners is that table, in reading order. A function rather than a
// package variable for the same reason groups() is one: nothing can mutate
// a caller's copy.
func osdCorners() []osdCorner {
	return []osdCorner{
		{"top left", osdNearEdge, osdNearEdge},
		// Top center: a TrackMix holds its timestamp at 65537,1 and draws it
		// centered along the top edge (seen on its own stream, 2026-10-23),
		// which is also the value reported in issue #10.
		{"top center", osdCenter, osdNearEdge},
		{"top right", osdFarEdge, osdNearEdge},
		{"bottom left", osdNearEdge, osdFarEdge},
		{"bottom right", osdFarEdge, osdFarEdge},
	}
}

// groups declares the curated Picture and OSD surface plus Lights and IR:
// what a person actually changes, so they do not have to edit raw XML.
//
// Every XPath here comes from testdata/livefixtures, a real RLC-810A's own
// replies (osd2.xml, isp.xml, led.xml), not from guessing at Reolink's
// schema: an earlier version of this file invented paths such as
// "Osd/osdChannel/name" and "Isp/Isp/bright" that do not exist in any
// document this camera actually sends (the real names are
// "OsdChannelName/name" and "VideoInput/bright"), which is exactly the
// kind of field that must be caught rather than papered over with a
// blank input box.
//
// The status LED (message 209) is here because it is proven; the
// floodlight is not a Field at all, because its write lives over CGI, not
// a Baichuan block, and cameraPage carries it separately.
//
// Lights and IR sets ConfirmReason because everything in it is an emitter:
// nothing in Picture and OSD switches anything a person or a camera can
// see happen, and this group is the opposite of that.
// The ORDER of this list is load-bearing beyond the page's layout:
// camerapage_test.go's fake camera replies to config reads in exactly this
// order, and baichuan.Conn reads strictly forward with no rewind, so
// reordering these groups makes that test time out rather than say what
// changed.
func groups() []Group {
	gs := curatedGroups()
	for i := range gs {
		if _, v, ok := strings.Cut(gs[i].Blocks[0], "#"); ok {
			for j := range gs[i].Fields {
				gs[i].Fields[j].Variant = v
			}
		}
	}
	return gs
}

func curatedGroups() []Group {
	return []Group{
		{
			Title: "Name and timestamp",
			// "osd get" (44) is the live document on the RLC-810A this was
			// verified against; "get osd" (29) answered 405 there but is a
			// second, independently recovered pair for the same settings
			// on other firmware (see Group's own comment). Listed in this
			// order because "osd get" is the one seen live; a camera that
			// only implements "get osd" still gets a working group, just
			// discovered rather than assumed.
			Blocks: []string{"osd get", "get osd"},
			Fields: []Field{
				{Label: "Camera name", XPath: "OsdChannelName/name", Kind: "text", Slug: "osd-name", NoCopy: true, Help: "The name drawn on the picture."},
				{Label: "Show camera name", XPath: "OsdChannelName/enable", Kind: "toggle", Slug: "osd-name-show", Governs: "osd-name-position"},
				// The Reolink logo burned into the picture. It lives in the
				// same OsdChannelName element as the name overlay (verified on
				// a TrackMix: <enWatermark>1</enWatermark>), so it rides the
				// same osd read/write pair. 0 removes it.
				{Label: "Reolink watermark", XPath: "OsdChannelName/enWatermark", Kind: "toggle", Slug: "osd-watermark", Help: "The Reolink logo drawn on the picture."},
				// Two elements, one control. See the corner table above for
				// what the values mean and how that was established.
				{Label: "Camera name position", XPath: "OsdChannelName/topLeftX", YPath: "OsdChannelName/topLeftY", Kind: "position", Slug: "osd-name-position"},
				{Label: "Show timestamp", XPath: "OsdDatetime/enable", Kind: "toggle", Slug: "osd-time-show", Governs: "osd-time-position"},
				{Label: "Timestamp position", XPath: "OsdDatetime/topLeftX", YPath: "OsdDatetime/topLeftY", Kind: "position", Slug: "osd-time-position"},
			},
		},
		{
			Title:  "Image",
			Blocks: []string{"isp get"},
			// Instant: brightness, contrast, saturation, color mode and flip
			// change the live picture without dropping the stream (watched
			// on a TrackMix and an RLC-810A, 2026-10-23).
			Fields: []Field{
				{Label: "Brightness", XPath: "VideoInput/bright", Kind: "number", Min: 0, Max: 255, Slug: "image-brightness"},
				{Label: "Contrast", XPath: "VideoInput/contrast", Kind: "number", Min: 0, Max: 255, Slug: "image-contrast"},
				{Label: "Saturation", XPath: "VideoInput/saturation", Kind: "number", Min: 0, Max: 255, Slug: "image-saturation"},
				{Label: "Day and night mode", XPath: "InputAdvanceCfg/DayNight/mode", Kind: "choice", Slug: "image-daynight", Help: "Auto switches to black and white with infrared when it gets dark.",
					Choices: []Choice{{"auto", "Auto"}, {"color", "Color"}, {"blackAndWhite", "Black & white"}}},
				// The picture's orientation, for a camera mounted upside down
				// or seen in a mirror. Same isp document; read on a TrackMix
				// (1,1, ceiling mounted) and an RLC-810A (0,0).
				{Label: "Flip horizontally", XPath: "InputAdvanceCfg/mirror", Kind: "toggle", Slug: "image-mirror", Help: "For a camera seen in a mirror or mounted facing the other way."},
				{Label: "Flip vertically", XPath: "InputAdvanceCfg/flip", Kind: "toggle", Slug: "image-flip", Help: "For a camera mounted upside down, on a ceiling."},
				// The two below were captured from the Reolink app writing
				// isp set (25) on a TrackMix, 2026-10-26.
				{Label: "Sharpness", XPath: "VideoInput/sharpen", Kind: "number", Min: 0, Max: 255, Slug: "image-sharpness"},
				// The frequency is ignored while enable is 0: an RLC-810A
				// answered OK to 60hz and kept 50hz (click test, 10-27).
				{Label: "Anti-flicker", XPath: "InputAdvanceCfg/PowerLineFrequency/enable", Kind: "toggle", Slug: "image-antiflicker-on", Governs: "image-antiflicker", Help: "Stops indoor lights flickering in the picture. Off suits a camera that only sees daylight."},
				{Label: "Mains frequency", XPath: "InputAdvanceCfg/PowerLineFrequency/mode", Kind: "choice", Slug: "image-antiflicker", Help: "Match your mains frequency: 60 Hz in the Americas, 50 Hz in most other places.",
					Choices: []Choice{{"50hz", "50 Hz"}, {"60hz", "60 Hz"}}},
			},
		},
		{
			Title:         "Lights",
			Blocks:        []string{"led get"},
			ConfirmReason: lightsConfirmReason,
			Fields: []Field{
				// LedState/state is a string enum ("auto" on the camera
				// this was verified against), not the 0/1 boolean this
				// field used to send: a numeric toggle would have written
				// "0" or "1" into a field the camera never uses those
				// values for. Stops for the firmware's spellings, and the
				// camera's own value is always offered as well, so a camera
				// holding something else is shown it rather than snapped.
				//
				// It is the INFRARED ILLUMINATOR, not the indicator on the
				// front, although this page called it "Status LED" until
				// 2026-10-08. The block names neither field, so the two
				// protocols were lined up against each other to settle it:
				//
				//   camera   LedState/state   CGI GetIrLights   LedState/lightState   CGI GetPowerLed
				//   fisheye  auto             Auto              open                  On
				//   pano     open             On                open                  On
				//
				// state tracks IrLights exactly. The mislabel mattered:
				// somebody switching off what reads as a small indicator
				// light was switching off the camera's night vision.
				{Label: "Infrared light", XPath: "LedState/state", Kind: "choice", Slug: "led-state", Help: "Night vision. Off leaves night images dark.",
					Choices: []Choice{{"auto", "Auto"}, {"open", "On"}, {"close", "Off"}}},
				// And the indicator itself, which this page never offered.
				// Same block, so it costs no extra read and is written the
				// same way. The camera's own CGI range for it is On/Off
				// with no auto, which is what a power indicator would have.
				{Label: "Status LED", XPath: "LedState/lightState", Kind: "choice", Slug: "led-light-state", Help: "The small light on the front of the camera.",
					Choices: []Choice{{"open", "On"}, {"close", "Off"}}},
			},
		},
		// Alerts. Every block below was read off a TrackMix (2026-10-23);
		// the on/off switches live in the *task blocks (ftptask, emailtask,
		// pushtask), not in the server settings. Each task block also holds
		// a 24x7 schedule per alarm type (valueTable), which these pages
		// leave as the camera has it. Passwords are left out on purpose:
		// a save reports the value it wrote, and a password must not be
		// put on screen.
		{
			Title:    "FTP upload",
			NeedsFTP: true,
			Blocks:   []string{"ftp task get"},
			Blurb:    "Sends clips and pictures on detection.",
			Untested: true,
			Fields: []Field{
				{Label: "Upload to FTP", XPath: "FtpTask/enable", Kind: "toggle", Slug: "ftp-enable", Help: "Uploads on detection, on the schedule set in the Reolink app."},
			},
		},
		{
			Title:    "FTP server",
			NeedsFTP: true,
			Blocks:   []string{"ftp cfg get"},
			Blurb:    "Set the password in the Reolink app.",
			Untested: true,
			Fields: []Field{
				{Label: "Server", XPath: "Ftp/server", Kind: "text", Slug: "ftp-server"},
				{Label: "Port", XPath: "Ftp/port", Kind: "text", Slug: "ftp-port"},
				{Label: "User name", XPath: "Ftp/userName", Kind: "text", Slug: "ftp-user"},
				{Label: "Folder", XPath: "Ftp/remoteDir", Kind: "text", Slug: "ftp-dir"},
				{Label: "Folder per day", XPath: "Ftp/autoDir", Kind: "toggle", Slug: "ftp-autodir", Help: "Puts each day in its own folder."},
			},
		},
		{
			Title:    "Email alerts",
			Blocks:   []string{"email task get"},
			Blurb:    "Emails on detection.",
			Untested: true,
			Fields: []Field{
				{Label: "Send email alerts", XPath: "EmailTask/enable", Kind: "toggle", Slug: "email-enable", Help: "Emails on detection, on the schedule set in the Reolink app."},
			},
		},
		{
			Title:    "Email server",
			Blocks:   []string{"email cfg get"},
			Blurb:    "Set the password in the Reolink app.",
			Untested: true,
			Fields: []Field{
				{Label: "SMTP server", XPath: "Email/smtpServer", Kind: "text", Slug: "email-server"},
				{Label: "SMTP port", XPath: "Email/smtpPort", Kind: "text", Slug: "email-port"},
				{Label: "Use SSL", XPath: "Email/ssl", Kind: "toggle", Slug: "email-ssl"},
				{Label: "Sender address", XPath: "Email/userName", Kind: "text", Slug: "email-user"},
				{Label: "Recipient 1", XPath: "Email/address1", Kind: "text", Slug: "email-to1"},
				{Label: "Recipient 2", XPath: "Email/address2", Kind: "text", Slug: "email-to2"},
				{Label: "Recipient 3", XPath: "Email/address3", Kind: "text", Slug: "email-to3"},
				// attachmentType is what the camera goes by; the older
				// attachment toggle beside it is ignored on an RLC-810A
				// (click test, 10-27), and missing altogether with video.
				{Label: "Attachment", XPath: "Email/attachmentType", Kind: "choice", Slug: "email-attach", Help: "What each alert email carries.",
					Choices: []Choice{{"none", "None"}, {"picture", "A picture"}, {"video", "A video clip"}}},
			},
		},
		{
			Title:    "Push notifications",
			Blocks:   []string{"push task get"},
			Blurb:    "To phones with the Reolink app.",
			Untested: true,
			Fields: []Field{
				{Label: "Send push notifications", XPath: "PushTask/enable", Kind: "toggle", Slug: "push-enable", Help: "Delivered through Reolink's cloud to the Reolink app."},
			},
		},
		// Everything below was captured from the Reolink Android app (4.63)
		// writing to a TrackMix, a Duo and a fisheye through reocapture on
		// 2026-10-26; see the settings map in the reocapture notes.
		{
			Title:    "Push timing",
			Optional: true,
			Blocks:   []string{"push cfg get"},
			Fields: []Field{
				{Label: "Time between notifications", XPath: "PushCfg/interval", Kind: "choice", Slug: "push-interval", Help: "The shortest gap between two notifications, in seconds.",
					Choices: []Choice{{"20", "20 s"}, {"30", "30 s"}, {"60", "1 min"}, {"120", "2 min"}}},
			},
		},
		{
			Title:    "Motion detection",
			Optional: true,
			Blocks:   []string{"md get"},
			Fields: []Field{
				{Label: "Motion detection", XPath: "MD/enable", Kind: "toggle", Slug: "md-enable", Governs: "md-sensitivity"},
				{Label: "Sensitivity", XPath: "MD/sensInfoNew/sensitivityDefault", Kind: "number", Min: 1, Max: 50, Invert: 51, Slug: "md-sensitivity", Help: "1 to 50. Higher notices smaller changes, like a person far away."},
			},
		},
		aiDetectGroup("Person detection", "people"),
		aiDetectGroup("Vehicle detection", "vehicle"),
		aiDetectGroup("Animal detection", "dog_cat"),
		{
			Title:    "Recording",
			Optional: true,
			Blocks:   []string{"rec task get"},
			Blurb:    "To the camera's own SD card.",
			Fields: []Field{
				{Label: "Record to SD card", XPath: "Record/enable", Kind: "toggle", Slug: "rec-enable"},
			},
		},
		{
			Title:    "Recording length",
			Optional: true,
			Blocks:   []string{"rec cfg get"},
			Fields: []Field{
				{Label: "Keep recording after motion ends", XPath: "RecordCfg/recordDelayTime", Kind: "choice", Slug: "rec-post",
					Choices: []Choice{{"15", "15 s"}, {"30", "30 s"}, {"60", "1 min"}}},
				{Label: "Overwrite oldest recordings", XPath: "RecordCfg/cycle", Kind: "toggle", Slug: "rec-cycle", Help: "When the card is full, record over the oldest files instead of stopping."},
			},
		},
		{
			Title:            "Record audio",
			Optional:         true,
			Blocks:           []string{"get enc"},
			InterruptsStream: true,
			Blurb:            "Turning this on or off restarts the camera's streams for a moment.",
			Fields: []Field{
				{Label: "Record audio", XPath: "mainStream/audio", Also: []string{"subStream/audio"}, Kind: "toggle", Slug: "audio-record", Help: "Sound in live view, in recordings and in every reostream stream."},
			},
		},
		{
			Title:    "Speaker",
			Optional: true,
			Blocks:   []string{"audio cfg get"},
			Fields: []Field{
				{Label: "Volume", XPath: "audioCfg/volume", Kind: "number", Min: 0, Max: 100, Slug: "audio-volume", Help: "0 to 100, for talk and the siren."},
				{Label: "Siren sound", XPath: "audioCfg/audioSelect", Kind: "choice", Slug: "audio-select", Help: "Custom is the sound uploaded below.",
					Choices: []Choice{{"0", "Default"}, {"1", "Custom"}}},
			},
		},
		{
			Title:    "Noise reduction",
			Optional: true,
			Blocks:   []string{"get ai denoise cfg"},
			Fields: []Field{
				{Label: "Noise reduction", XPath: "aiDenoise/enable", Kind: "toggle", Slug: "audio-denoise", Governs: "audio-denoise-level"},
				{Label: "Level", XPath: "aiDenoise/level", Kind: "number", Slug: "audio-denoise-level"},
			},
		},
		{
			Title:         "Siren on detection",
			Optional:      true,
			Blocks:        []string{"audio task get"},
			ConfirmReason: "The siren sounds out loud on every detection while this is on.",
			Fields: []Field{
				{Label: "Sound the siren on detection", XPath: "AudioTask/enable", Kind: "toggle", Slug: "siren-enable"},
			},
		},
		{
			Title:           "Spotlight",
			Optional:        true,
			NeedsFloodlight: true,
			Blocks:          []string{"floodlight task get"},
			ConfirmReason:   lightsConfirmReason,
			Fields: []Field{
				{Label: "Brightness", XPath: "FloodlightTask/brightness_cur", Kind: "number", Min: 0, Max: 100, Slug: "spot-brightness"},
				{Label: "Schedule starts (hour)", XPath: "FloodlightTask/schedule/startHour", Kind: "number", Min: 0, Max: 23, Slug: "spot-start"},
				{Label: "Schedule ends (hour)", XPath: "FloodlightTask/schedule/endHour", Kind: "number", Min: 0, Max: 23, Slug: "spot-end"},
			},
		},
		// The four below were proved on a TrackMix by flipping each, reading
		// it back and restoring it (2026-10-26).
		{
			Title:         "Remote access",
			Blocks:        []string{"get ptop"},
			Optional:      true,
			ConfirmReason: "Turning Reolink P2P off stops the Reolink app reaching this camera by its UID, including from outside your network.",
			Fields: []Field{
				{Label: "Reolink P2P (UID access)", XPath: "PTOP/enable", Kind: "toggle", Slug: "net-p2p", Help: "How the Reolink app reaches the camera without its IP address. reostream does not need it."},
			},
		},
		{
			Title:    "UPnP",
			Blocks:   []string{"get upnp"},
			Optional: true,
			Fields: []Field{
				{Label: "UPnP port forwarding", XPath: "Upnp/enable", Kind: "toggle", Slug: "net-upnp", Help: "Lets the camera open ports on your router by itself. Usually best off."},
			},
		},
		{
			Title:    "Firmware updates",
			Blocks:   []string{"get auto update"},
			Optional: true,
			Fields: []Field{
				{Label: "Update automatically", XPath: "AutoUpdate/enable", Kind: "toggle", Slug: "sys-autoupdate", Help: "The camera installs new firmware from Reolink by itself."},
			},
		},
		{
			Title:    "Login lockout",
			Blocks:   []string{"get general"},
			Optional: true,
			Fields: []Field{
				{Label: "Lock out after failed logins", XPath: "SystemGeneral/loginLock", Kind: "toggle", Slug: "sys-loginlock", Help: "After several wrong passwords within 3 minutes, the camera refuses logins for a few minutes."},
			},
		},
		{
			Title:    "Tracking method",
			Optional: true,
			Blocks:   []string{"ai cfg get"},
			Fields: []Field{
				{Label: "How it follows", XPath: "AiCfg/smartTrackMode", Kind: "choice", Slug: "track-mode", Help: "Digital zooms the second lens only; pan/tilt turns the camera.",
					Choices: []Choice{{"2", "Digital only"}, {"3", "Digital first"}, {"4", "Pan/tilt first"}}},
			},
		},
	}
}

// pairForName finds the read/write pair whose firmware description is
// name, the same wording Group.Blocks and PairRow.Name already use. It is
// how one of a Group's Blocks candidates gets resolved back to the ids a
// handler needs to seed and write its fields.
func pairForName(name string) (baichuan.ConfigPair, bool) {
	name, _, _ = strings.Cut(name, "#")
	for _, p := range baichuan.ConfigPairs() {
		if p.Name == name {
			return p, true
		}
	}
	return baichuan.ConfigPair{}, false
}

// aiDetectGroup is one object type's own detection settings. The camera
// keeps one AiDetectCfg per type and reads it with the type named in the
// request, hence the variant block.
func aiDetectGroup(title, kind string) Group {
	return Group{
		Title:    title,
		Optional: true,
		Blocks:   []string{"ai detect cfg get#" + kind},
		Fields: []Field{
			{Label: "Sensitivity", XPath: "AiDetectCfg/sensitivity", Kind: "number", Min: 0, Max: 100, Slug: "ai-" + kind + "-sensitivity", Help: "0 to 100. Higher catches more, and more false alarms."},
			{Label: "Alarm delay (seconds)", XPath: "AiDetectCfg/stayTime", Kind: "number", Min: 0, Max: 8, Slug: "ai-" + kind + "-delay", Help: "How long it has to be there before it counts. 0 to 8."},
		},
	}
}

// blockRequest is the request document a variant block is read with (see
// Group.Blocks: "name#variant"), or nil for a block read with only its
// channel.
func blockRequest(name string, channel int) []byte {
	base, v, ok := strings.Cut(name, "#")
	if !ok {
		return nil
	}
	switch base {
	case "ai detect cfg get":
		// The Reolink app's own request, captured 2026-10-26.
		return []byte(fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n<AiDetectCfg version=\"1.1\">\n<chn>%d</chn>\n<type>%s</type>\n</AiDetectCfg>\n</body>\n", channel, v))
	}
	return nil
}

// expandAlso adds, for each edit of a field with Also paths, the same
// value written to each of them.
func expandAlso(block string, edits []edit) ([]edit, error) {
	out := append([]edit(nil), edits...)
	for _, e := range edits {
		f, ok := curatedField(block, e.XPath)
		if !ok {
			return nil, fmt.Errorf("%s %s is not a curated field", block, e.XPath)
		}
		if e.XPath != f.XPath {
			continue
		}
		for _, a := range f.Also {
			out = append(out, edit{XPath: a, Value: e.Value})
		}
	}
	return out, nil
}

// setField locates the element xpath names and replaces only its text
// content with value.
//
// This edits bytes, not a decoded structure. Unmarshalling into a struct
// and re-marshalling is exactly what loses the XML declaration, reorders
// elements, changes whitespace, and drops every field this code does not
// model, and formatting is not cosmetic here: a compact document is
// accepted for TalkAbility and refused for TalkConfig. The camera supplies
// its own schema, including fields nobody here has ever modeled, and
// echoing its own document back byte for byte except for the one field
// being changed is what makes writes work on models nobody here has seen.
//
// An element that is not present is refused rather than invented: adding an
// element the camera never sent is composing a document, which is the
// thing this function exists to avoid.
func setField(doc []byte, xpath, value string) ([]byte, error) {
	if xpath == "" {
		return nil, fmt.Errorf("control: setField: empty xpath")
	}
	segs := strings.Split(xpath, "/")

	start, end, err := locateLeaf(doc, segs)
	if err != nil {
		return nil, fmt.Errorf("control: setField %q: %w", xpath, err)
	}

	var esc bytes.Buffer
	if err := xml.EscapeText(&esc, []byte(value)); err != nil {
		return nil, fmt.Errorf("control: setField %q: escaping value: %w", xpath, err)
	}

	out := make([]byte, 0, len(doc)+esc.Len())
	out = append(out, doc[:start]...)
	out = append(out, esc.Bytes()...)
	out = append(out, doc[end:]...)
	return out, nil
}

// span is one candidate match locateLeaf found: the byte range of an
// element's inner text.
type span struct{ start, end int }

// locateLeaf finds the byte span of the inner text of the element chain
// path names, matched as a trailing suffix of the document's current
// element stack, so a caller never has to spell out whatever the document
// wraps it in.
//
// It reads doc as a stream of tokens rather than building a tree, using
// only the decoder's own byte offsets to find where a match's content
// starts and ends. Nothing here reconstructs or reorders any byte setField
// hands back to its caller; the decoder is used purely to find offsets into
// the original, untouched bytes.
//
// Matching by suffix is what lets a caller's path skip an outer wrapper it
// was never told about, but the same rule means a short path such as
// "Isp/bright" matches every ancestor chain ending in those two names. A
// document with that pair once, say per channel, would make the first
// match a silent guess at which channel the caller meant: not a refusal,
// a wrong write, which is the one failure this codebase cares most about
// not producing. So this scans the whole document rather than stopping at
// the first hit, and refuses when more than one element matches, on the
// same principle that refuses an element that is not present at all: a
// path this code cannot resolve to exactly one element is not information
// this code has, and it must not guess.
func locateLeaf(doc []byte, path []string) (start, end int, err error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	var matches []span
	matchDepth := -1
	pendingStart := false
	var curStart int

	for {
		off := int(dec.InputOffset())
		tok, tokErr := dec.Token()
		if tokErr != nil {
			break
		}
		if pendingStart {
			curStart = off
			pendingStart = false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			// matchDepth == -1 also guards against a match already open:
			// once one is found, nothing inside its own subtree can start
			// a second, independent match, only a sibling or a later
			// element can.
			if matchDepth == -1 && suffixMatches(stack, path) {
				matchDepth = len(stack)
				pendingStart = true
			}
		case xml.EndElement:
			if matchDepth == len(stack) {
				// A self-closing element such as <enable/> gives the
				// decoder no separate open and close tags: the synthetic
				// EndElement it emits lands at the same offset as the end
				// of the StartElement token, which is indistinguishable by
				// offset alone from a genuinely empty "<a></a>". The bytes
				// tell them apart: only a self-closing tag ends in "/>".
				// Splicing at that offset would not insert text inside the
				// element at all, it would insert it after the element, as
				// a sibling, which is exactly the kind of invented content
				// this function exists to refuse rather than produce.
				if bytes.HasSuffix(doc[:curStart], []byte("/>")) {
					return 0, 0, fmt.Errorf("element is self-closing and has no text span to replace")
				}
				matches = append(matches, span{curStart, off})
				matchDepth = -1
			}
			stack = stack[:len(stack)-1]
		}
	}

	switch len(matches) {
	case 0:
		return 0, 0, fmt.Errorf("element not found in document")
	case 1:
		return matches[0].start, matches[0].end, nil
	default:
		return 0, 0, fmt.Errorf("%d elements match this path; refusing rather than guessing which one was meant", len(matches))
	}
}

// suffixMatches reports whether path is the trailing sequence of stack, so
// "Osd/osdChannel/name" matches whether the document wraps it in <body>,
// a channel container, or nothing at all.
func suffixMatches(stack, path []string) bool {
	if len(stack) < len(path) {
		return false
	}
	off := len(stack) - len(path)
	for i, name := range path {
		if stack[off+i] != name {
			return false
		}
	}
	return true
}

// fieldValue reads what setField would replace, without replacing it: the
// current text of the element xpath names, for seeding a form with what the
// camera actually holds right now. It shares locateLeaf with setField, so a
// value shown on the page and a value setField would refuse are found by
// exactly the same rule, including refusing an ambiguous match rather than
// guessing which one to display.
func fieldValue(doc []byte, xpath string) (string, bool) {
	if xpath == "" {
		return "", false
	}
	start, end, err := locateLeaf(doc, strings.Split(xpath, "/"))
	if err != nil {
		return "", false
	}
	return string(doc[start:end]), true
}

// resolveGroupBlock tries each of g.Blocks against conn, in order, and
// returns the first one that both names a real read/write pair and reads
// back 200: this is what discovers, rather than assumes, which of two
// independently recovered pairs for the same settings this camera actually
// implements (see Group's own comment on Blocks). failReason is empty
// exactly when pair and doc are usable; otherwise it names why every
// candidate was rejected, for a caller to show in place of a control.
func resolveGroupBlock(ctx context.Context, conn *baichuan.Conn, g Group) (pair baichuan.ConfigPair, doc []byte, failReason string) {
	var lastReason string
	for _, name := range g.Blocks {
		p, ok := pairForName(name)
		if !ok {
			lastReason = fmt.Sprintf("%q is not a known read/write pair", name)
			continue
		}
		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		d, status, err := baichuan.ReadConfigWith(readCtx, conn, p.Get, blockRequest(name, conn.Channel()))
		readCancel()
		if err != nil {
			lastReason = fmt.Sprintf("reading %s: %v", name, err)
			continue
		}
		if status != 200 {
			lastReason = fmt.Sprintf("%s: %s", name, baichuan.ExplainStatus(status))
			continue
		}
		// The name as the group listed it, variant and all ("ai detect cfg
		// get#people"): it is what the form posts back as the block, and a
		// write without the variant is refused as not a curated field.
		p.Name = name
		return p, d, ""
	}
	if lastReason == "" {
		lastReason = "no candidate block is configured for this group"
	}
	return baichuan.ConfigPair{}, nil, fmt.Sprintf("this camera did not answer any of %s (%s)", strings.Join(g.Blocks, ", "), lastReason)
}

// curatedField reports whether block/xpath together name one of the fields
// groups() actually declares, and returns it.
//
// The settings form posts both as plain form values, and nothing about an
// HTTP request stops it from naming any block and any path. This is the
// check that a write from this form can only ever land on a field this
// page itself curated, never on whatever a request happens to claim.
func curatedField(block, xpath string) (Field, bool) {
	for _, g := range groups() {
		if !slices.Contains(g.Blocks, block) {
			continue
		}
		for _, f := range g.Fields {
			// Every path the field edits, not only its primary one: a
			// position field's topLeftY is exactly as curated as its
			// topLeftX, and a write naming it must pass this check rather
			// than be rejected as something the page never declared.
			for _, p := range f.Paths() {
				if p == xpath {
					return f, true
				}
			}
		}
	}
	return Field{}, false
}

// edit is one element a write changes: where it is and what it becomes.
type edit struct {
	XPath string
	Value string
}

// parseEdits turns the settings form's xpath and value into the list of
// element changes one write must carry.
//
// One control, one form, one write -- but some controls are more than one
// element. An overlay's position is topLeftX AND topLeftY, and a browser
// select can only post a single value, so the form names its elements in
// xpath as a comma-separated list (Field.FormXPath) and posts the values in
// the same order. A form naming one element posts exactly the two plain
// strings it always did and comes back out of here as exactly one edit.
//
// The xpath list governs the split, never the value. A single-element write
// is not split at all, so a camera name containing a comma still arrives
// whole; a two-element write splits the value into exactly two parts. A
// value list that does not have one part per path is refused rather than
// padded: half a position is a corner nobody chose.
func parseEdits(xpathList, valueList string) ([]edit, error) {
	paths := strings.Split(xpathList, ",")
	if len(paths) == 1 {
		return []edit{{XPath: paths[0], Value: valueList}}, nil
	}
	values := strings.SplitN(valueList, ",", len(paths))
	if len(values) != len(paths) {
		return nil, fmt.Errorf("this control changes %d fields together but %d values were posted", len(paths), len(values))
	}
	edits := make([]edit, len(paths))
	for i := range paths {
		edits[i] = edit{XPath: paths[i], Value: values[i]}
	}
	return edits, nil
}

// serveApplySetting is the settings form's POST: read the field's block
// fresh, replace the field or fields being changed with setField, and write
// the result through writeBlock, the same path serveWrite already uses for
// the raw view.
//
// One read, every change, one write. A control that edits two elements at
// once (an overlay's corner) must not become two read-modify-write round
// trips: a camera that accepted the first and refused the second would
// leave the overlay in a corner nobody picked. So every edit is applied to
// the one document this handler read, and if any of them cannot be applied
// nothing is sent at all -- setField is pure, so the whole document is
// either finished or abandoned before a single byte goes to the camera.
//
// This is not a second write path. Nothing here calls baichuan.WriteConfig
// itself; writeBlock does, which is what makes a curated edit inherit
// verification, the confirmed/accepted/refused vocabulary, the
// pre-write document capture a caller can restore from, and the
// unsafe-to-rewrite warning, all for free and all exactly as a raw edit
// already gets them.
//
// The field seeded on the page and the field a write targets are found by
// the identical rule on purpose: fieldValue and setField both resolve
// XPath through locateLeaf, so what an operator sees is what gets changed.
func (s *Server) serveApplySetting(w http.ResponseWriter, r *http.Request) {
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
	block := r.FormValue("block")

	edits, err := parseEdits(r.FormValue("xpath"), r.FormValue("value"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Every posted path is checked against groups() before anything is
	// read, for the same reason one always was: nothing about an HTTP
	// request stops it naming any path at all, and a write from this form
	// may only ever land on a field this page itself curated. A list makes
	// that check a loop, not a weaker rule.
	var field Field
	for i, e := range edits {
		f, ok := curatedField(block, e.XPath)
		if !ok {
			http.Error(w, fmt.Sprintf("%s %s is not a curated field", block, e.XPath), http.StatusBadRequest)
			return
		}
		if i == 0 {
			field = f
		}
	}
	for i, e := range edits {
		f, _ := curatedField(block, e.XPath)
		v, err := f.stored(e.Value)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		edits[i].Value = v
	}
	if edits, err = expandAlso(block, edits); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pair, ok := pairForName(block)
	if !ok {
		http.Error(w, fmt.Sprintf("%q is not a known block", block), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	// Read the block fresh, on its own connection, so setField starts from
	// what the camera holds right now rather than from whatever the page
	// happened to render it with a request or two ago. writeBlock reads
	// this same block again for its own Before capture; that second read
	// is not wasted work, it is what lets writeBlock report Before/After
	// without this handler having to hand it anything but a finished body.
	conn, err := s.dial(ctx, cam)
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s before writing: %v", pair.Name, err), http.StatusBadGateway)
		return
	}
	readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
	req := blockRequest(block, conn.Channel())
	doc, status, err := baichuan.ReadConfigWith(readCtx, conn, pair.Get, req)
	readCancel()
	conn.Close()
	if err != nil {
		http.Error(w, fmt.Sprintf("reading %s before writing: %v", pair.Name, err), http.StatusBadGateway)
		return
	}
	if status != 200 {
		http.Error(w, fmt.Sprintf("reading %s before writing: camera answered status %d", pair.Name, status), http.StatusBadGateway)
		return
	}

	// Where this write reports back to. The camera page is where a curated
	// setting lives, but the status page's quick overlay switches post
	// through this same handler, and an operator toggling one on the fleet
	// view expects to still be on the fleet view afterwards; returnTo lets
	// that form say where it came from, and ignores anything that is not a
	// path on this page.
	back := returnTo(r, "/cameras/"+url.PathEscape(cam.Name))
	subject := fmt.Sprintf("%s: %s", cam.Name, lowerFirst(field.Label))

	// Every edit onto the one document just read, in order. Each setField
	// returns a fresh slice, so a later one that fails leaves the earlier
	// ones nowhere but in a local nobody sends.
	body := doc
	for _, e := range edits {
		next, setErr := setField(body, e.XPath, e.Value)
		if setErr != nil {
			// The inferred image XPaths in particular may not match this
			// model's actual schema. That must read as a refusal, the same
			// vocabulary a rejected write already uses, never as a silent
			// success: nothing was sent to the camera at all. For a
			// multi-element control this is also what keeps it whole --
			// one unappliable half refuses the entire change rather than
			// writing the other half on its own.
			s.setFlash(w, r, flashFor(subject, WriteResult{
				Outcome: "refused",
				Detail:  fmt.Sprintf("could not apply %s to the current document: %v", e.XPath, setErr),
			}))
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		body = next
	}

	result, err := s.writeBlockWith(ctx, cam, pair.Set, body, true, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// A whole-document comparison cannot tell a camera that reformatted
	// what it kept from one that kept nothing. The fields this form edits
	// can: read each one back. All of them as sent is saved; any of them
	// still at its old value is not, whatever the camera answered.
	if result.Outcome == "accepted" && len(result.After) > 0 {
		kept, stale := true, ""
		for _, e := range edits {
			got, ok := fieldValue(result.After, e.XPath)
			if !ok || strings.TrimSpace(got) != strings.TrimSpace(e.Value) {
				kept = false
				if ok {
					stale = strings.TrimSpace(got)
				}
				break
			}
		}
		if kept {
			result.Outcome, result.Detail = "confirmed", "read back on a fresh connection: the camera holds the new value."
		} else {
			result.Outcome = "refused"
			result.Detail = "the camera answered OK but did not keep it"
			if stale != "" {
				result.Detail += " (it still reports " + stale + ")"
			}
		}
	}

	// Post/redirect/get rather than rendering the write's own result page.
	// A refresh after this is a plain GET of the page the setting lives
	// on, not a second write to the camera, and the operator is left
	// looking at the control they just changed rather than at a wall of
	// before/after XML whose only way onward was "restore previous".
	flash := flashFor(subject, result)
	if len(result.Before) > 0 {
		// The same verified restore result.html offers, built the same
		// way: the document this block held before the write, sent back
		// through /write/{id} with verify on. Only the presentation
		// changes.
		flash.UndoAction = fmt.Sprintf("/cameras/%s/write/%d", cam.Name, pair.Set)
		flash.UndoParam = "body"
		flash.UndoValue = string(result.Before)
		// Carry the page this write came from into the undo itself, so
		// pressing undo lands back here with a banner rather than on the
		// block editor's before/after page.
		flash.UndoHidden = map[string]string{"verify": "true", "return": back}
	}
	s.setFlash(w, r, flash)
	http.Redirect(w, r, back, http.StatusSeeOther)
}
