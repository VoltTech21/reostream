package stream

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"runtime/debug"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	mch264 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mch265 "github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// nativePaths are the streams a camera also serves over its own RTSP
// server. The TrackMix's zoom lens is the one, as Preview_02_main (1920x1080,
// H.264 or H.265 as its encoder is set). Run fetches it over Baichuan (see zoomChannel in stream.go); this
// path is the fallback for a camera whose zoom never sends video that way.
var nativePaths = map[string]string{
	"zoom":     "Preview_02_main",
	"zoom_sub": "Preview_02_sub",
}

// nativeRTSPPort is where Reolink cameras serve RTSP. reostream turns the
// service on when it adds a camera (baichuan.EnableServices), which keeps
// this fallback available.
var nativeRTSPPort = "554"

// runNative pulls one stream from the camera's own RTSP server and
// publishes it exactly as Run publishes a Baichuan stream: the same muxer,
// hub and sink, so every output treats it like any other stream.
func runNative(ctx context.Context, cfg Config, h *hub.Hub, path string) (err error) {
	defer h.MarkDisconnected()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("stream: %s: panic: %v\n%s", cfg.Name, r, debug.Stack())
			err = fmt.Errorf("stream: %s: panic: %v", cfg.Name, r)
		}
	}()

	host := cfg.Address
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	u, err := base.ParseURL("rtsp://" + url.UserPassword(cfg.Username, cfg.Password).String() + "@" +
		net.JoinHostPort(host, nativeRTSPPort) + "/" + path)
	if err != nil {
		return fmt.Errorf("stream: %s: %w", cfg.Name, err)
	}
	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	if err := c.Start(); err != nil {
		return fmt.Errorf("stream: %s: rtsp: %w", cfg.Name, err)
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		return fmt.Errorf("stream: %s: rtsp describe: %w (is RTSP on for this camera?)", cfg.Name, err)
	}
	frames := make(chan baichuan.Frame, 64)
	medi, onPacket, err := nativeVideo(c, desc, frames)
	if err != nil {
		return fmt.Errorf("stream: %s: %w", cfg.Name, err)
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return fmt.Errorf("stream: %s: rtsp setup: %w", cfg.Name, err)
	}
	onPacket()
	if _, err := c.Play(nil); err != nil {
		return fmt.Errorf("stream: %s: rtsp play: %w", cfg.Name, err)
	}

	h.MarkConnected()
	if cs, ok := cfg.Sink.(ConnectionSink); ok {
		cs.Connected()
	}
	mediaTimeout := cfg.MediaTimeout
	if mediaTimeout <= 0 {
		mediaTimeout = DefaultMediaTimeout
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	timer := time.NewTimer(mediaTimeout)
	defer timer.Stop()
	var mux *ts.Muxer
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return fmt.Errorf("stream: %s: rtsp: %w", cfg.Name, err)
		case <-timer.C:
			return fmt.Errorf("stream: %s: no video frame in %s from the camera's RTSP server", cfg.Name, mediaTimeout)
		case f := <-frames:
			timer.Reset(mediaTimeout)
			if mux == nil {
				if f.Kind != baichuan.FrameIFrame {
					continue
				}
				// Video only: no audio is carried from the camera's RTSP,
				// and a declared audio track that never gets any data
				// leaves a browser's player waiting for it, never playing.
				nm, err := ts.NewMuxer(f.Codec)
				if err != nil {
					return fmt.Errorf("stream: %s: %w", cfg.Name, err)
				}
				mux = nm
				h.SetHeader(mux.Header())
			}
			if cfg.Sink != nil {
				cfg.Sink.Frame(f)
			}
			if pkt, key := mux.FrameWithKey(f); pkt != nil {
				h.PublishKey(pkt, key)
				h.RecordFrame(len(pkt))
			}
			if f.Kind == baichuan.FrameIFrame {
				h.SetKeyframe(f.Codec, f.Video())
			}
		}
	}
}

func annexB(nalus [][]byte) []byte {
	n := 0
	for _, u := range nalus {
		n += 4 + len(u)
	}
	out := make([]byte, 0, n)
	for _, u := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, u...)
	}
	return out
}

// nativeVideo finds the video in a native stream's description and returns
// it with a function that starts turning its RTP into frames on out. The
// zoom lens follows its own encoder setting, so it may be H.264 or H.265:
// firmware v3.0.0.5428 defaults the TrackMix zoom to H.265 (emulated
// 2026-10-29). Each keyframe gets the parameter sets in front, as a
// Baichuan keyframe has them.
func nativeVideo(c *gortsplib.Client, desc *description.Session, out chan<- baichuan.Frame) (*description.Media, func(), error) {
	send := func(pts int64, codec string, au [][]byte, key bool) {
		f := baichuan.Frame{Kind: baichuan.FramePFrame, Codec: codec, Data: annexB(au), Micros: uint32(pts * 1_000_000 / 90_000)}
		if key {
			f.Kind = baichuan.FrameIFrame
		}
		select {
		case out <- f:
		default: // a stalled reader drops frames rather than the session
		}
	}

	var h264 *format.H264
	if medi := desc.FindFormat(&h264); medi != nil {
		dec, err := h264.CreateDecoder()
		if err != nil {
			return nil, nil, err
		}
		sps, pps := h264.SPS, h264.PPS
		return medi, func() {
			c.OnPacketRTP(medi, h264, func(pkt *rtp.Packet) {
				// Every packet, not just the ones that finish a frame:
				// the clock is unwrapped packet by packet.
				pts, ok := c.PacketPTS(medi, pkt)
				if !ok {
					return
				}
				au, err := dec.Decode(pkt)
				if err != nil || len(au) == 0 {
					return
				}
				key := false
				for _, n := range au {
					switch n[0] & 0x1f {
					case 5:
						key = true
					case 7:
						sps = n
					case 8:
						pps = n
					}
				}
				if key && sps != nil && pps != nil {
					au = append([][]byte{sps, pps}, au...)
				}
				send(pts, "H264", au, key)
			})
		}, nil
	}

	var h265 *format.H265
	if medi := desc.FindFormat(&h265); medi != nil {
		dec, err := h265.CreateDecoder()
		if err != nil {
			return nil, nil, err
		}
		vps, sps, pps := h265.VPS, h265.SPS, h265.PPS
		return medi, func() {
			c.OnPacketRTP(medi, h265, func(pkt *rtp.Packet) {
				// Every packet, not just the ones that finish a frame:
				// the clock is unwrapped packet by packet.
				pts, ok := c.PacketPTS(medi, pkt)
				if !ok {
					return
				}
				au, err := dec.Decode(pkt)
				if err != nil || len(au) == 0 {
					return
				}
				key := false
				for _, n := range au {
					switch typ := (n[0] >> 1) & 0x3f; {
					case typ >= 16 && typ <= 21: // IRAP: BLA, IDR, CRA
						key = true
					case typ == 32:
						vps = n
					case typ == 33:
						sps = n
					case typ == 34:
						pps = n
					}
				}
				if key && vps != nil && sps != nil && pps != nil {
					au = append([][]byte{vps, sps, pps}, au...)
				}
				send(pts, "H265", au, key)
			})
		}, nil
	}
	return nil, nil, fmt.Errorf("the camera's RTSP stream has no H.264 or H.265 video")
}

// NativeInfo asks the camera's own RTSP server what one native stream is,
// without playing it: its codec and picture size from the DESCRIBE answer.
// An error means the camera does not serve it (or RTSP is off).
func NativeInfo(ctx context.Context, address, username, password, stream string) (codec string, width, height int, err error) {
	path, ok := nativePaths[stream]
	if !ok {
		return "", 0, 0, fmt.Errorf("%q is not a native stream", stream)
	}
	host := address
	if hh, _, e := net.SplitHostPort(host); e == nil {
		host = hh
	}
	u, err := base.ParseURL("rtsp://" + url.UserPassword(username, password).String() + "@" +
		net.JoinHostPort(host, nativeRTSPPort) + "/" + path)
	if err != nil {
		return "", 0, 0, err
	}
	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	if dl, ok := ctx.Deadline(); ok {
		c.ReadTimeout, c.WriteTimeout = time.Until(dl), time.Until(dl)
	}
	if err := c.Start(); err != nil {
		return "", 0, 0, err
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		return "", 0, 0, err
	}
	var h264 *format.H264
	if desc.FindFormat(&h264) != nil {
		var sps mch264.SPS
		if len(h264.SPS) > 0 && sps.Unmarshal(h264.SPS) == nil {
			width, height = sps.Width(), sps.Height()
		}
		return "H264", width, height, nil
	}
	var h265 *format.H265
	if desc.FindFormat(&h265) != nil {
		var sps mch265.SPS
		if len(h265.SPS) > 0 && sps.Unmarshal(h265.SPS) == nil {
			width, height = sps.Width(), sps.Height()
		}
		return "H265", width, height, nil
	}
	return "", 0, 0, fmt.Errorf("no H.264 or H.265 video in the camera's %s", path)
}
