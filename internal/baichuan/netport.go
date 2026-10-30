package baichuan

import (
	"context"
	"encoding/xml"
	"fmt"
)

// Message ids for the camera's network-service (ServerPort) config. GET is a
// single empty document; SET must use the two-section SetConfig framing, or
// the camera acknowledges it and changes nothing.
const (
	MsgIDGetNetPort = 37
	MsgIDSetNetPort = 36
)

// NetPort is which network services a camera serves and on which ports. RTSP
// is the one that matters here: it is the fallback for a second-lens camera
// (TrackMix) whose zoom lens sends no video over Baichuan, and many cameras
// ship with RTSP off.
type NetPort struct {
	ServerPort PortService
	HTTP       PortService
	HTTPS      PortService
	RTSP       PortService
	ONVIF      PortService
	RTMP       PortService
}

// PortService is one service's port and whether it is enabled.
type PortService struct {
	Port    int
	Enabled bool
}

type portBlock struct {
	Enable int `xml:"enable"`
	// Each block names its port differently; capture any of them.
	ServerPort int `xml:"serverPort"`
	HTTPPort   int `xml:"httpPort"`
	HTTPSPort  int `xml:"httpsPort"`
	RTSPPort   int `xml:"rtspPort"`
	ONVIFPort  int `xml:"onvifPort"`
	RTMPPort   int `xml:"rtmpPort"`
}

type netPortDoc struct {
	XMLName xml.Name   `xml:"body"`
	Server  *portBlock `xml:"ServerPort"`
	HTTP    *portBlock `xml:"HttpPort"`
	HTTPS   *portBlock `xml:"HttpsPort"`
	RTSP    *portBlock `xml:"RtspPort"`
	ONVIF   *portBlock `xml:"OnvifPort"`
	RTMP    *portBlock `xml:"RtmpPort"`
}

// GetNetPort reads the camera's network-service configuration.
func (c *Conn) GetNetPort(ctx context.Context) (NetPort, error) {
	if err := c.SendDocument(MsgIDGetNetPort, nil); err != nil {
		return NetPort{}, err
	}
	xmlBody, status, err := awaitReply(ctx, c, MsgIDGetNetPort)
	if err != nil {
		return NetPort{}, err
	}
	if status != 0 && status != 200 {
		return NetPort{}, fmt.Errorf("baichuan: get netport: camera answered status %d", status)
	}
	return parseNetPort(xmlBody)
}

func parseNetPort(b []byte) (NetPort, error) {
	var d netPortDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		return NetPort{}, fmt.Errorf("baichuan: parse netport: %w", err)
	}
	var np NetPort
	if d.Server != nil {
		np.ServerPort = PortService{d.Server.ServerPort, d.Server.Enable != 0}
	}
	if d.HTTP != nil {
		np.HTTP = PortService{d.HTTP.HTTPPort, d.HTTP.Enable != 0}
	}
	if d.HTTPS != nil {
		np.HTTPS = PortService{d.HTTPS.HTTPSPort, d.HTTPS.Enable != 0}
	}
	if d.RTSP != nil {
		np.RTSP = PortService{d.RTSP.RTSPPort, d.RTSP.Enable != 0}
	}
	if d.ONVIF != nil {
		np.ONVIF = PortService{d.ONVIF.ONVIFPort, d.ONVIF.Enable != 0}
	}
	if d.RTMP != nil {
		np.RTMP = PortService{d.RTMP.RTMPPort, d.RTMP.Enable != 0}
	}
	return np, nil
}

// Services names the network services to ensure are on. A false field is
// left exactly as the camera has it; EnableServices never turns anything off.
type Services struct {
	HTTP  bool
	HTTPS bool
	RTSP  bool
	ONVIF bool
	RTMP  bool
}

// UsefulServices is the set worth enabling for an NVR: RTSP to stream (and
// the fallback for a TrackMix zoom lens), ONVIF for PTZ and discovery, and
// HTTP for the camera's own settings page.
var UsefulServices = Services{HTTP: true, RTSP: true, ONVIF: true}

// EnableRTSP turns RTSP on if it is off, leaving everything else as it is.
func (c *Conn) EnableRTSP(ctx context.Context) (bool, error) {
	return c.EnableServices(ctx, Services{RTSP: true})
}

// EnableServices turns on each requested service that is off, leaving the
// rest untouched, and reports whether it changed anything. The camera takes
// its ports as two groups (ServerPort/Http/Https and Rtsp/Onvif/Rtmp), so a
// change touches at most one write per group, each carrying its whole group.
func (c *Conn) EnableServices(ctx context.Context, want Services) (changed bool, err error) {
	np, err := c.GetNetPort(ctx)
	if err != nil {
		return false, err
	}
	// Desired state: on where either already on or asked for; never off.
	httpOn := np.HTTP.Enabled || want.HTTP
	httpsOn := np.HTTPS.Enabled || want.HTTPS
	rtspOn := np.RTSP.Enabled || want.RTSP
	onvifOn := np.ONVIF.Enabled || want.ONVIF
	rtmpOn := np.RTMP.Enabled || want.RTMP

	if rtspOn != np.RTSP.Enabled || onvifOn != np.ONVIF.Enabled || rtmpOn != np.RTMP.Enabled {
		body := streamGroupDoc(np, rtspOn, onvifOn, rtmpOn)
		if err := c.writeNetPort(ctx, body); err != nil {
			return changed, err
		}
		changed = true
	}
	if httpOn != np.HTTP.Enabled || httpsOn != np.HTTPS.Enabled {
		body := webGroupDoc(np, httpOn, httpsOn)
		if err := c.writeNetPort(ctx, body); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// SetServices sets each service exactly as want says, on or off, and reports
// whether it changed anything. Ports are kept as the camera has them. Same
// two-group write as EnableServices; the media port (ServerPort, 9000) is
// never touched, since it is how reostream itself reaches the camera.
func (c *Conn) SetServices(ctx context.Context, want Services) (changed bool, err error) {
	np, err := c.GetNetPort(ctx)
	if err != nil {
		return false, err
	}
	if want.RTSP != np.RTSP.Enabled || want.ONVIF != np.ONVIF.Enabled || want.RTMP != np.RTMP.Enabled {
		if err := c.writeNetPort(ctx, streamGroupDoc(np, want.RTSP, want.ONVIF, want.RTMP)); err != nil {
			return changed, err
		}
		changed = true
	}
	if want.HTTP != np.HTTP.Enabled || want.HTTPS != np.HTTPS.Enabled {
		if err := c.writeNetPort(ctx, webGroupDoc(np, want.HTTP, want.HTTPS)); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// SetPorts writes np's ports and on/off states, one write per group that
// differs from what the camera holds now. The media port (ServerPort) is
// never changed: it is how reostream itself reaches the camera.
func (c *Conn) SetPorts(ctx context.Context, np NetPort) error {
	cur, err := c.GetNetPort(ctx)
	if err != nil {
		return err
	}
	np.ServerPort = cur.ServerPort
	if np.RTSP != cur.RTSP || np.ONVIF != cur.ONVIF || np.RTMP != cur.RTMP {
		if err := c.writeNetPort(ctx, streamGroupDoc(np, np.RTSP.Enabled, np.ONVIF.Enabled, np.RTMP.Enabled)); err != nil {
			return err
		}
	}
	if np.HTTP != cur.HTTP || np.HTTPS != cur.HTTPS {
		if err := c.writeNetPort(ctx, webGroupDoc(np, np.HTTP.Enabled, np.HTTPS.Enabled)); err != nil {
			return err
		}
	}
	return nil
}

// writeNetPort sends a ServerPort write and waits for the camera's ack. The
// write is applied asynchronously, so a caller that closes the connection the
// instant SetConfig returns can race the camera and lose the change.
func (c *Conn) writeNetPort(ctx context.Context, body []byte) error {
	if err := c.SetConfig(MsgIDSetNetPort, body); err != nil {
		return fmt.Errorf("baichuan: set netport: %w", err)
	}
	if _, status, err := awaitReply(ctx, c, MsgIDSetNetPort); err != nil {
		return fmt.Errorf("baichuan: set netport: awaiting ack: %w", err)
	} else if status != 0 && status != 200 {
		return fmt.Errorf("baichuan: set netport: camera answered status %d", status)
	}
	return nil
}

// streamGroupDoc is the Rtsp/Onvif/Rtmp write; webGroupDoc the Http/Https
// one (carrying ServerPort, which the camera groups with them). Both use the
// byte-exact XML declaration the camera's own client sends: a differing
// declaration is silently ignored (see the TalkConfig note in the protocol
// docs). Ports that read back as zero fall back to Reolink's defaults.
func streamGroupDoc(np NetPort, rtsp, onvif, rtmp bool) []byte {
	return netPortDocXML(
		block("RtspPort", "rtspPort", orDefault(np.RTSP.Port, 554), rtsp),
		block("OnvifPort", "onvifPort", orDefault(np.ONVIF.Port, 8000), onvif),
		block("RtmpPort", "rtmpPort", orDefault(np.RTMP.Port, 1935), rtmp),
	)
}

func webGroupDoc(np NetPort, http, https bool) []byte {
	return netPortDocXML(
		block("ServerPort", "serverPort", orDefault(np.ServerPort.Port, 9000), np.ServerPort.Enabled),
		block("HttpPort", "httpPort", orDefault(np.HTTP.Port, 80), http),
		block("HttpsPort", "httpsPort", orDefault(np.HTTPS.Port, 443), https),
	)
}

func netPortDocXML(blocks ...string) []byte {
	s := "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n"
	for _, b := range blocks {
		s += b
	}
	return []byte(s + "</body>\n")
}

func block(tag, portTag string, port int, enabled bool) string {
	return fmt.Sprintf("<%s version=\"1.1\">\n<%s>%d</%s>\n<enable>%d</enable>\n</%s>\n",
		tag, portTag, port, portTag, boolBit(enabled), tag)
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func boolBit(b bool) int {
	if b {
		return 1
	}
	return 0
}
