// Package onvif serves the small slice of ONVIF a recorder needs to steer a
// camera that only speaks Baichuan: device discovery, one media profile with
// a PTZ configuration, continuous pan/tilt, stop, and presets.
//
// It exists for Frigate's `onvif:` section, which is how Frigate moves a
// camera and how its PTZ controls appear in the UI. It is not a general
// ONVIF device: there is no event service, no imaging, no recording, and
// video is not offered here at all. Streaming stays on RTSP.
//
// A Reolink camera moves in the four straight directions only (a diagonal
// command is accepted and ignored by the camera, see docs/ptz.md), so a
// velocity with both axes set moves along the stronger one. Speed is passed
// through and is ignored by cameras that have one fixed speed.
package onvif

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// PTZ is what the server needs from a camera's motors. internal/ptz's
// Controller is the production implementation.
type PTZ interface {
	Move(ctx context.Context, cmd baichuan.PTZCommand, speed int) error
	Stop(ctx context.Context) error
	Presets(ctx context.Context) ([]baichuan.PTZPreset, error)
	GotoPreset(ctx context.Context, id int) error
	Moving() bool
}

// Options configures one camera's ONVIF endpoint.
type Options struct {
	// Name is the camera's name, used in the device information.
	Name string

	// Username and Password, when set, are required of every request except
	// GetSystemDateAndTime, as ONVIF itself requires. Left empty the endpoint
	// accepts anyone who can reach it, so bind it to a trusted interface.
	Username string
	Password string

	// Width and Height describe the video source in the media profile. They
	// are informational; zero means 1920x1080.
	Width, Height int

	// MaxTimeout caps a ContinuousMove timeout. Zero means 60 seconds.
	MaxTimeout time.Duration

	// Now is the clock for the UsernameToken freshness check; nil is time.Now.
	Now func() time.Time
}

// Handler is the http.Handler for one camera's ONVIF endpoint. It answers on
// any path: ONVIF clients are told the service addresses by
// GetCapabilities and GetServices, and every address leads here.
type Handler struct {
	ptz  PTZ
	opts Options
}

// NewHandler returns the handler for one camera.
func NewHandler(p PTZ, opts Options) *Handler {
	if opts.Width == 0 || opts.Height == 0 {
		opts.Width, opts.Height = 1920, 1080
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 60 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{ptz: p, opts: opts}
}

// Fixed identifiers. A camera has one video source, one encoder, one PTZ
// node and one profile, so these never vary.
const (
	profileToken  = "profile_main"
	sourceToken   = "video_source"
	encoderToken  = "video_encoder"
	ptzConfToken  = "ptz_config"
	ptzNodeToken  = "ptz_node"
	velocitySpace = "http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"
)

const (
	nsEnvelope = "http://www.w3.org/2003/05/soap-envelope"
	nsDevice   = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia    = "http://www.onvif.org/ver10/media/wsdl"
	nsPTZ      = "http://www.onvif.org/ver20/ptz/wsdl"
	nsSchema   = "http://www.onvif.org/ver10/schema"
)

type envelope struct {
	Header struct {
		Security struct {
			Token struct {
				Username string `xml:"Username"`
				Password struct {
					Type  string `xml:"Type,attr"`
					Value string `xml:",chardata"`
				} `xml:"Password"`
				Nonce   string `xml:"Nonce"`
				Created string `xml:"Created"`
			} `xml:"UsernameToken"`
		} `xml:"Security"`
	} `xml:"Header"`
	Body struct {
		Inner []byte `xml:",innerxml"`
	} `xml:"Body"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "ONVIF is SOAP over POST", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	var env envelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		writeFault(w, http.StatusBadRequest, "s:Sender", "ter:InvalidArgVal", "malformed SOAP envelope")
		return
	}
	dec := xml.NewDecoder(bytes.NewReader(env.Body.Inner))
	var start xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			writeFault(w, http.StatusBadRequest, "s:Sender", "ter:InvalidArgVal", "empty SOAP body")
			return
		}
		if se, ok := tok.(xml.StartElement); ok {
			start = se
			break
		}
	}
	op := start.Name.Local

	if op != "GetSystemDateAndTime" && !h.authorised(&env) {
		writeFault(w, http.StatusUnauthorized, "s:Sender", "ter:NotAuthorized", "sender not authorized")
		return
	}

	base := "http://" + r.Host
	ctx := r.Context()
	var body string
	switch op {
	case "GetSystemDateAndTime":
		body = h.systemDateAndTime()
	case "GetCapabilities":
		body = h.capabilities(base)
	case "GetServices":
		body = h.services(base)
	case "GetServiceCapabilities":
		body = h.serviceCapabilities(start.Name.Space)
	case "GetDeviceInformation":
		body = h.deviceInformation()
	case "GetProfiles":
		body = `<trt:GetProfilesResponse>` + h.profile("trt:Profiles") + `</trt:GetProfilesResponse>`
	case "GetProfile":
		body = `<trt:GetProfileResponse>` + h.profile("trt:Profile") + `</trt:GetProfileResponse>`
	case "GetVideoSources":
		body = h.videoSources()
	case "GetNodes":
		body = `<tptz:GetNodesResponse>` + h.node("tptz:PTZNode") + `</tptz:GetNodesResponse>`
	case "GetNode":
		body = `<tptz:GetNodeResponse>` + h.node("tptz:PTZNode") + `</tptz:GetNodeResponse>`
	case "GetConfigurations":
		body = `<tptz:GetConfigurationsResponse>` + h.ptzConfiguration("tptz:PTZConfiguration") + `</tptz:GetConfigurationsResponse>`
	case "GetConfiguration":
		body = `<tptz:GetConfigurationResponse>` + h.ptzConfiguration("tptz:PTZConfiguration") + `</tptz:GetConfigurationResponse>`
	case "GetConfigurationOptions":
		body = h.configurationOptions()
	case "GetStatus":
		body = h.status()
	case "GetPresets":
		ps, err := h.ptz.Presets(ctx)
		if err != nil {
			writeFault(w, http.StatusInternalServerError, "s:Receiver", "ter:Action", err.Error())
			return
		}
		body = presetsResponse(ps)
	case "GotoPreset":
		var req struct {
			PresetToken string `xml:"PresetToken"`
		}
		_ = dec.DecodeElement(&req, &start)
		id, err := strconv.Atoi(strings.TrimSpace(req.PresetToken))
		if err != nil {
			writeFault(w, http.StatusBadRequest, "s:Sender", "ter:InvalidArgVal", "unknown preset token")
			return
		}
		if err := h.ptz.GotoPreset(ctx, id); err != nil {
			writeFault(w, http.StatusInternalServerError, "s:Receiver", "ter:Action", err.Error())
			return
		}
		body = `<tptz:GotoPresetResponse/>`
	case "ContinuousMove":
		if fault := h.continuousMove(ctx, dec, &start); fault != nil {
			writeFault(w, fault.status, fault.code, fault.subcode, fault.reason)
			return
		}
		body = `<tptz:ContinuousMoveResponse/>`
	case "Stop":
		if err := h.ptz.Stop(ctx); err != nil {
			writeFault(w, http.StatusInternalServerError, "s:Receiver", "ter:Action", err.Error())
			return
		}
		body = `<tptz:StopResponse/>`
	default:
		writeFault(w, http.StatusBadRequest, "s:Receiver", "ter:ActionNotSupported", "not supported: "+op)
		return
	}
	writeEnvelope(w, body)
}

type fault struct {
	status        int
	code, subcode string
	reason        string
}

// continuousMove handles ContinuousMove. A velocity of zero on both axes is
// a stop. Zoom is ignored: no Reolink pan/tilt model has a zoom motor, and
// the PTZ configuration does not advertise one.
func (h *Handler) continuousMove(ctx context.Context, dec *xml.Decoder, start *xml.StartElement) *fault {
	var req struct {
		Velocity struct {
			PanTilt struct {
				X float64 `xml:"x,attr"`
				Y float64 `xml:"y,attr"`
			} `xml:"PanTilt"`
		} `xml:"Velocity"`
		Timeout string `xml:"Timeout"`
	}
	if err := dec.DecodeElement(&req, start); err != nil {
		return &fault{http.StatusBadRequest, "s:Sender", "ter:InvalidArgVal", "malformed ContinuousMove"}
	}
	x, y := req.Velocity.PanTilt.X, req.Velocity.PanTilt.Y
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return &fault{http.StatusBadRequest, "s:Sender", "ter:InvalidArgVal", "velocity is not a number"}
	}
	if x == 0 && y == 0 {
		if err := h.ptz.Stop(ctx); err != nil {
			return &fault{http.StatusInternalServerError, "s:Receiver", "ter:Action", err.Error()}
		}
		return nil
	}
	cmd, mag := direction(x, y)
	if err := h.ptz.Move(ctx, cmd, speedFor(mag)); err != nil {
		return &fault{http.StatusInternalServerError, "s:Receiver", "ter:Action", err.Error()}
	}
	if t, ok := parseDuration(req.Timeout); ok && t > 0 {
		if t > h.opts.MaxTimeout {
			t = h.opts.MaxTimeout
		}
		// The camera has no timed move, so the stop is ours to send. A
		// later command supersedes it harmlessly: Stop is idempotent.
		time.AfterFunc(t, func() {
			c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = h.ptz.Stop(c)
		})
	}
	return nil
}

// direction picks the straight-line command for a velocity. ONVIF's y axis
// points up and its x axis points right.
func direction(x, y float64) (baichuan.PTZCommand, float64) {
	if math.Abs(x) >= math.Abs(y) {
		if x < 0 {
			return baichuan.PTZLeft, math.Abs(x)
		}
		return baichuan.PTZRight, math.Abs(x)
	}
	if y > 0 {
		return baichuan.PTZUp, math.Abs(y)
	}
	return baichuan.PTZDown, math.Abs(y)
}

// speedFor maps an ONVIF velocity magnitude of 0..1 onto the camera's 1..64.
func speedFor(mag float64) int {
	s := int(math.Round(mag * 64))
	if s < 1 {
		s = 1
	}
	if s > 64 {
		s = 64
	}
	return s
}

var durationRE = regexp.MustCompile(`^PT(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)

// parseDuration reads the xs:duration forms a PTZ client sends, PT5S and
// PT1M30S, and nothing else.
func parseDuration(s string) (time.Duration, bool) {
	m := durationRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || (m[1] == "" && m[2] == "") {
		return 0, false
	}
	var d time.Duration
	if m[1] != "" {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * time.Minute
	}
	if m[2] != "" {
		f, _ := strconv.ParseFloat(m[2], 64)
		d += time.Duration(f * float64(time.Second))
	}
	return d, true
}

// authorised checks the WS-Security UsernameToken when credentials are
// configured. Both digest and plain text passwords are accepted: a client
// chooses, and digest is what Frigate sends.
func (h *Handler) authorised(env *envelope) bool {
	if h.opts.Username == "" && h.opts.Password == "" {
		return true
	}
	t := env.Header.Security.Token
	if subtle.ConstantTimeCompare([]byte(t.Username), []byte(h.opts.Username)) != 1 {
		return false
	}
	if strings.HasSuffix(t.Password.Type, "PasswordDigest") {
		nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(t.Nonce))
		if err != nil {
			return false
		}
		created, err := time.Parse(time.RFC3339, strings.TrimSpace(t.Created))
		if err != nil {
			return false
		}
		if d := h.opts.Now().Sub(created); d > 5*time.Minute || d < -5*time.Minute {
			return false
		}
		sum := sha1.New()
		sum.Write(nonce)
		sum.Write([]byte(strings.TrimSpace(t.Created)))
		sum.Write([]byte(h.opts.Password))
		want := base64.StdEncoding.EncodeToString(sum.Sum(nil))
		return subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(t.Password.Value))) == 1
	}
	return subtle.ConstantTimeCompare([]byte(t.Password.Value), []byte(h.opts.Password)) == 1
}

func writeEnvelope(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", `application/soap+xml; charset=utf-8`)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<s:Envelope xmlns:s=%q xmlns:tds=%q xmlns:trt=%q xmlns:tptz=%q xmlns:tt=%q xmlns:ter="http://www.onvif.org/ver10/error">`+
		`<s:Body>%s</s:Body></s:Envelope>`,
		nsEnvelope, nsDevice, nsMedia, nsPTZ, nsSchema, body)
}

func writeFault(w http.ResponseWriter, status int, code, subcode, reason string) {
	w.Header().Set("Content-Type", `application/soap+xml; charset=utf-8`)
	w.WriteHeader(status)
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(reason))
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<s:Envelope xmlns:s=%q xmlns:ter="http://www.onvif.org/ver10/error">`+
		`<s:Body><s:Fault><s:Code><s:Value>%s</s:Value><s:Subcode><s:Value>%s</s:Value></s:Subcode></s:Code>`+
		`<s:Reason><s:Text xml:lang="en">%s</s:Text></s:Reason></s:Fault></s:Body></s:Envelope>`,
		nsEnvelope, code, subcode, esc.String())
}

func (h *Handler) systemDateAndTime() string {
	t := h.opts.Now().UTC()
	return fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime>`+
		`<tt:DateTimeType>NTP</tt:DateTimeType><tt:DaylightSavings>false</tt:DaylightSavings>`+
		`<tt:TimeZone><tt:TZ>UTC0</tt:TZ></tt:TimeZone>`+
		`<tt:UTCDateTime><tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>`+
		`<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date></tt:UTCDateTime>`+
		`</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`,
		t.Hour(), t.Minute(), t.Second(), t.Year(), int(t.Month()), t.Day())
}

func (h *Handler) capabilities(base string) string {
	return fmt.Sprintf(`<tds:GetCapabilitiesResponse><tds:Capabilities>`+
		`<tt:Device><tt:XAddr>%[1]s/onvif/device_service</tt:XAddr></tt:Device>`+
		`<tt:Media><tt:XAddr>%[1]s/onvif/media_service</tt:XAddr>`+
		`<tt:StreamingCapabilities><tt:RTPMulticast>false</tt:RTPMulticast><tt:RTP_TCP>true</tt:RTP_TCP><tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP></tt:StreamingCapabilities></tt:Media>`+
		`<tt:PTZ><tt:XAddr>%[1]s/onvif/ptz_service</tt:XAddr></tt:PTZ>`+
		`</tds:Capabilities></tds:GetCapabilitiesResponse>`, base)
}

func (h *Handler) services(base string) string {
	svc := func(ns, path string) string {
		return fmt.Sprintf(`<tds:Service><tds:Namespace>%s</tds:Namespace><tds:XAddr>%s/onvif/%s</tds:XAddr>`+
			`<tds:Version><tt:Major>2</tt:Major><tt:Minor>0</tt:Minor></tds:Version></tds:Service>`, ns, base, path)
	}
	return `<tds:GetServicesResponse>` + svc(nsDevice, "device_service") +
		svc(nsMedia, "media_service") + svc(nsPTZ, "ptz_service") + `</tds:GetServicesResponse>`
}

// serviceCapabilities answers GetServiceCapabilities for whichever service
// the request was addressed to; the namespace of the element says which.
func (h *Handler) serviceCapabilities(ns string) string {
	switch ns {
	case nsPTZ:
		// MoveStatus is true so a client may poll GetStatus; StatusPosition
		// is false because the position is not reported in ONVIF units.
		return `<tptz:GetServiceCapabilitiesResponse><tptz:Capabilities EFlip="false" Reverse="false" GetCompatibleConfigurations="false" MoveStatus="true" StatusPosition="false"/></tptz:GetServiceCapabilitiesResponse>`
	case nsMedia:
		return `<trt:GetServiceCapabilitiesResponse><trt:Capabilities SnapshotUri="false" Rotation="false" VideoSourceMode="false" OSD="false"/></trt:GetServiceCapabilitiesResponse>`
	default:
		return `<tds:GetServiceCapabilitiesResponse><tds:Capabilities/></tds:GetServiceCapabilitiesResponse>`
	}
}

func (h *Handler) deviceInformation() string {
	return fmt.Sprintf(`<tds:GetDeviceInformationResponse><tds:Manufacturer>reostream</tds:Manufacturer>`+
		`<tds:Model>%s</tds:Model><tds:FirmwareVersion>1</tds:FirmwareVersion>`+
		`<tds:SerialNumber>%s</tds:SerialNumber><tds:HardwareId>%s</tds:HardwareId></tds:GetDeviceInformationResponse>`,
		esc(h.opts.Name), esc(h.opts.Name), esc(h.opts.Name))
}

func (h *Handler) videoSources() string {
	return fmt.Sprintf(`<trt:GetVideoSourcesResponse><trt:VideoSources token=%q>`+
		`<tt:Framerate>15</tt:Framerate><tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`+
		`</trt:VideoSources></trt:GetVideoSourcesResponse>`, sourceToken, h.opts.Width, h.opts.Height)
}

// profile is the camera's only media profile. element is the wrapping tag,
// which differs between GetProfiles and GetProfile.
func (h *Handler) profile(element string) string {
	return fmt.Sprintf(`<%[1]s token=%[2]q fixed="true"><tt:Name>%[3]s</tt:Name>`+
		`<tt:VideoSourceConfiguration token=%[4]q><tt:Name>%[4]s</tt:Name><tt:UseCount>1</tt:UseCount>`+
		`<tt:SourceToken>%[4]s</tt:SourceToken><tt:Bounds x="0" y="0" width="%[6]d" height="%[7]d"/></tt:VideoSourceConfiguration>`+
		`<tt:VideoEncoderConfiguration token=%[5]q><tt:Name>%[5]s</tt:Name><tt:UseCount>1</tt:UseCount>`+
		`<tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>%[6]d</tt:Width><tt:Height>%[7]d</tt:Height></tt:Resolution>`+
		`<tt:Quality>5</tt:Quality><tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl>`+
		`<tt:SessionTimeout>PT60S</tt:SessionTimeout></tt:VideoEncoderConfiguration>`+
		`%[8]s</%[1]s>`,
		element, profileToken, esc(h.opts.Name), sourceToken, encoderToken,
		h.opts.Width, h.opts.Height, h.ptzConfiguration("tt:PTZConfiguration"))
}

func (h *Handler) ptzConfiguration(element string) string {
	return fmt.Sprintf(`<%[1]s token=%[2]q><tt:Name>PTZ</tt:Name><tt:UseCount>1</tt:UseCount>`+
		`<tt:NodeToken>%[3]s</tt:NodeToken>`+
		`<tt:DefaultContinuousPanTiltVelocitySpace>%[4]s</tt:DefaultContinuousPanTiltVelocitySpace>`+
		`<tt:DefaultPTZTimeout>PT10S</tt:DefaultPTZTimeout></%[1]s>`,
		element, ptzConfToken, ptzNodeToken, velocitySpace)
}

func (h *Handler) node(element string) string {
	return fmt.Sprintf(`<%[1]s token=%[2]q FixedHomePosition="false"><tt:Name>PTZ</tt:Name>`+
		`<tt:SupportedPTZSpaces><tt:ContinuousPanTiltVelocitySpace><tt:URI>%[3]s</tt:URI>`+
		`<tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange><tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange>`+
		`</tt:ContinuousPanTiltVelocitySpace></tt:SupportedPTZSpaces>`+
		`<tt:MaximumNumberOfPresets>64</tt:MaximumNumberOfPresets><tt:HomeSupported>false</tt:HomeSupported></%[1]s>`,
		element, ptzNodeToken, velocitySpace)
}

func (h *Handler) configurationOptions() string {
	return fmt.Sprintf(`<tptz:GetConfigurationOptionsResponse><tptz:PTZConfigurationOptions><tt:Spaces>`+
		`<tt:ContinuousPanTiltVelocitySpace><tt:URI>%s</tt:URI>`+
		`<tt:XRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:XRange><tt:YRange><tt:Min>-1</tt:Min><tt:Max>1</tt:Max></tt:YRange>`+
		`</tt:ContinuousPanTiltVelocitySpace></tt:Spaces>`+
		`<tt:PTZTimeout><tt:Min>PT0S</tt:Min><tt:Max>PT%dS</tt:Max></tt:PTZTimeout>`+
		`</tptz:PTZConfigurationOptions></tptz:GetConfigurationOptionsResponse>`,
		velocitySpace, int(h.opts.MaxTimeout/time.Second))
}

func (h *Handler) status() string {
	move := "IDLE"
	if h.ptz.Moving() {
		move = "MOVING"
	}
	return fmt.Sprintf(`<tptz:GetStatusResponse><tptz:PTZStatus><tt:MoveStatus><tt:PanTilt>%s</tt:PanTilt></tt:MoveStatus>`+
		`<tt:UtcTime>%s</tt:UtcTime></tptz:PTZStatus></tptz:GetStatusResponse>`,
		move, h.opts.Now().UTC().Format("2006-01-02T15:04:05Z"))
}

func presetsResponse(ps []baichuan.PTZPreset) string {
	var b strings.Builder
	b.WriteString(`<tptz:GetPresetsResponse>`)
	for _, p := range ps {
		name := p.Name
		if name == "" {
			name = "preset " + strconv.Itoa(p.ID)
		}
		fmt.Fprintf(&b, `<tptz:Preset token="%d"><tt:Name>%s</tt:Name></tptz:Preset>`, p.ID, esc(name))
	}
	b.WriteString(`</tptz:GetPresetsResponse>`)
	return b.String()
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
