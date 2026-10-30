// Package stream joins one camera to one hub: dial, depacketize, mux,
// publish, on repeat until the context is canceled or something fails.
package stream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// pingInterval matches the cadence bcprobe proved keeps a session alive.
// The camera logs "session:%u login timeout" and drops a connection it stops
// hearing from well before this, so this is not tuned close to a limit.
const pingInterval = 10 * time.Second

// DefaultMediaTimeout bounds how long Run tolerates zero decoded video
// frames before giving up on the connection and returning an error, so the
// supervisor tears it down and reconnects.
//
// This exists because internal/baichuan's idle-read timeout does not catch
// the failure that actually happened in production on 2026-09-08: a camera
// holding a stale session from a client that died without sending
// stream-stop still answers a new login, hands out a valid nonce, and keeps
// the connection responsive at the byte level (it acks other protocol
// traffic), while never sending a single media frame. Bytes kept arriving,
// so the idle timeout's read deadline kept getting pushed out, and two
// streams sat reporting connected:true, 0 fps, restarts:0 for over 25
// seconds with no reconnection attempt before this was caught by hand. Byte
// liveness and media liveness are different facts about a connection, and
// only media liveness is what this package's job depends on.
//
// 30 seconds is chosen from the fleet measurements taken the same day,
// against the whole running fleet, not a lab bench: every healthy stream
// observed ran between 9.9 and 25.1 fps with last_frame_age_seconds
// consistently under 0.1, including a
// 4K main stream whose keyframes span several messages and a substream
// under load. None of that comes anywhere close to even a single second of
// genuine gap between decoded frames. 30s is a full order of magnitude
// above the worst observed gap, not a value tuned close to it, so it will
// not fire on a camera stuttering under load, a keyframe taking longer to
// reassemble, or ordinary network jitter. It is also double
// DefaultIdleTimeout: a fully silent camera (no bytes at all, not even
// keepalive traffic) is always caught by the idle-read timeout first, since
// that fires at 15s: this timeout exists specifically to catch the case the
// idle timeout structurally cannot, a connection with traffic but no media,
// so it does not need to race it and can afford to be patient. The
// production incident this responds to ran for minutes before anyone
// noticed; catching it in 30s instead is a large improvement without
// risking a reconnect storm on a healthy fleet.
const DefaultMediaTimeout = 30 * time.Second

// Config names one camera stream to run.
// FrameSink receives every frame a stream decodes, before it is muxed.
//
// It exists so an output that needs frames rather than TS bytes, such as
// RTSP, can have them without the hub carrying frames and without demuxing
// TS back into them. Implementations must not block: this runs on the
// camera goroutine, and anything slow here stalls the read loop that the
// media watchdog is watching.
type FrameSink interface {
	Frame(baichuan.Frame)
}

// ConnectionSink is a FrameSink that also wants to know when the camera
// connection feeding it has been established, which it is told once per
// connection, before any frame from that connection.
//
// A sink whose state outlives one connection needs this. The RTSP output is
// one: its session, and the timestamp clock behind it, stay up while the
// camera reconnects, and after a camera reboot the camera's microsecond
// counter has started over. A clock that is not told treats that as either
// a stall or a wrap, and a reader sees timestamps that freeze for up to 35
// minutes or jump forward by up to 71. The HTTP output has no such need
// because it builds a new muxer per connection.
type ConnectionSink interface {
	FrameSink
	Connected()
}

// SpeakerSink is a sink that offers two-way audio and so needs to know,
// before the first frame, whether the camera has a speaker.
type SpeakerSink interface {
	SetSpeaker(bool)
}

type Config struct {
	Name     string
	Address  string
	Username string
	Password string
	Stream   string // "main", "sub" or "extern"
	Channel  int    // the camera's channel on a hub or NVR; 0 on its own
	UID      string // a battery camera's UID; empty for one on TCP

	// HoldPicture is the camera's hold_picture setting (see config.Camera).
	HoldPicture bool

	// Rotate is the camera's rotate setting (see config.Camera); nil
	// leaves it to what the camera says at login.
	Rotate *int

	// IdleTimeout overrides baichuan.DefaultIdleTimeout. A battery camera
	// that is awake sends within a couple of seconds, so one that does not
	// is holding a session and is quicker to retry than to wait out.
	IdleTimeout time.Duration

	// Sink, when non-nil, receives every decoded frame before muxing. Nil
	// is the default and the only state the HTTP output has ever run in.
	Sink FrameSink

	// MediaTimeout overrides DefaultMediaTimeout. Zero means the default.
	//
	// This is a Config field, not a TOML setting: unlike a per-camera value
	// such as a stream name or credentials, the right timeout here is a
	// property of the protocol's own frame cadence (see
	// DefaultMediaTimeout's derivation from fleet-wide fps measurements),
	// the same reasoning that kept baichuan.Options.IdleTimeout out of the
	// TOML config. Exposing it to operators would invite "fixing" a camera
	// that is actually failing by loosening the timeout that exists to
	// catch exactly that, which defeats the point of the watchdog. It stays
	// a Go field so tests can shorten it without a live camera or a config
	// file.
	MediaTimeout time.Duration
}

// streamKind maps the config's short stream name to the wire value
// StartVideo expects.
func streamKind(name string) (string, bool) {
	switch name {
	case "", "main":
		return baichuan.StreamMain, true
	case "sub":
		return baichuan.StreamSub, true
	case "extern":
		return baichuan.StreamExtern, true
	default:
		return "", false
	}
}

// Run dials cfg's camera, starts its video stream, and publishes muxed
// MPEG-TS to h until ctx is canceled or an error occurs. It never retries:
// retry and backoff belong to the supervisor that runs many of these, since
// a runner that retries internally would hold the camera's one connection
// for that stream and block its own replacement from ever connecting.
//
// Run returns promptly on an already canceled context without dialling, so
// a supervisor can cancel a Config before Run has had a chance to start it.
func Run(ctx context.Context, cfg Config, h *hub.Hub) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Whatever ends this attempt, whether the stream fails outright or the
	// supervisor is about to back off and retry, the fps and bitrate gauges
	// must not keep reporting the last rate seen while nothing is actually
	// arriving. DroppedAudio and AudioFrames are untouched by this: those
	// are cumulative counters meant to survive a reconnect, not a rate.
	defer h.MarkDisconnected()

	zoom := isZoom(cfg.Stream)
	if path, ok := nativePaths[cfg.Stream]; ok && (!zoom || zoomOverRTSP(cfg.Name)) {
		return runNative(ctx, cfg, h, path)
	}

	kind, ok := streamKind(cfg.Stream)
	if zoom {
		kind, ok = baichuan.StreamMain, true
	}
	if !ok {
		return fmt.Errorf("stream: %s: unknown stream %q", cfg.Name, cfg.Stream)
	}

	opts := baichuan.Options{
		Username:    cfg.Username,
		Password:    cfg.Password,
		Channel:     cfg.Channel,
		UID:         cfg.UID,
		IdleTimeout: cfg.IdleTimeout,
	}
	// Behind an NVR or Home Hub the stream is a reader on a connection
	// other streams share (see baichuan.Shared); a camera on its own still
	// gets a connection of its own, conn, and src is that connection.
	var conn *baichuan.Conn
	var sub *baichuan.Sub
	var src source
	if p := currentPool(); p != nil {
		channel, want := cfg.Channel, kind
		if zoom {
			// A recorder offers no lens of its own to unlock: the zoom
			// is asked for as the channel it has always been asked for,
			// without channel 0's sub stream, which on a recorder is
			// another camera's picture.
			channel = zoomChannel
			if cfg.Stream == "zoom_sub" {
				want = baichuan.StreamSub
			}
		}
		sub, conn, err = p.Open(ctx, cfg.Address, opts, channel, want)
	} else {
		conn, err = baichuan.Dial(ctx, cfg.Address, opts)
	}
	if err != nil {
		return fmt.Errorf("stream: %s: dial: %w", cfg.Name, err)
	}
	if sub != nil {
		src = sub
		// Close leaves the connection to the streams still on it, and
		// stops this one's preview if nobody else is reading it.
		defer sub.Close()
	} else {
		src = conn
		// Close unconditionally on every return path. Close sends the
		// stream-stop message that releases the camera's session; skipping
		// it leaves the camera refusing new connections on this stream for
		// minutes.
		defer conn.Close()
	}

	// net/http recovers a handler panic on its own, but nothing does that
	// for this goroutine. Registered after defer conn.Close() above, so it
	// runs first on the way out: it stops the panic and sets err, then
	// conn.Close() runs as normal right after, same as any other error
	// return. Without this, a panic anywhere in the loop below still
	// unwinds through conn.Close() (Go runs defers during a panic), but then
	// keeps going and crashes the whole process, taking down every other
	// camera the supervisor is running, not just this one.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("stream: %s: panic: %v\n%s", cfg.Name, r, debug.Stack())
			err = fmt.Errorf("stream: %s: panic: %v", cfg.Name, r)
		}
	}()

	// A camera that encodes its picture sideways says so at login; see
	// rotate.go. Nothing is logged for the flag itself: an upright camera
	// sends it too, so only a picture that is actually turned is logged.
	rotate, rotateFromCamera, _ := rotationFor(cfg, src.DeviceInfo())
	var infoW, infoH int
	rotateLogged := false

	// Ask before StartVideo, while nothing else is arriving: the answer has
	// to be in before the first keyframe fixes the sink's description. A
	// camera whose Support block cannot be read is left as it was rather
	// than failing the stream, since video does not depend on it.
	if ss, ok := cfg.Sink.(SpeakerSink); ok {
		var sup baichuan.Support
		var err error
		if sub != nil {
			sup, err = sub.Support()
		} else {
			sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			sup, err = baichuan.GetSupport(sctx, conn)
			cancel()
		}
		if err == nil {
			ss.SetSpeaker(sup.CanTalk())
		}
	}

	switch {
	case sub != nil:
		// Subscribing already asked for the stream.
	case zoom:
		if err := startZoom(conn, cfg.Stream); err != nil {
			return fmt.Errorf("stream: %s: start video: %w", cfg.Name, err)
		}
	default:
		if err := conn.StartVideo(kind); err != nil {
			return fmt.Errorf("stream: %s: start video: %w", cfg.Name, err)
		}
	}
	gotVideo := false

	mediaTimeout := cfg.MediaTimeout
	if mediaTimeout <= 0 {
		mediaTimeout = DefaultMediaTimeout
	}

	// A connection is only worth anything to the hub's clients once the
	// camera has actually been asked for video, so the connected mark and
	// the watchdog's own clock both start here, not at Dial. This is also
	// what catches the 2026-09-08 incident's exact shape: a stream that has
	// never produced a single frame from the moment it connected, not just
	// one that stops after a while. Starting lastMedia at StartVideo,
	// before the first frame has any chance to arrive, means a camera that
	// never sends one is timed out on the same footing as one that stops
	// partway through, rather than needing a separate "never started" check.
	h.MarkConnected()
	if cs, ok := cfg.Sink.(ConnectionSink); ok {
		cs.Connected()
	}
	lastMedia := time.Now()

	d := baichuan.NewDepacketizer()
	// waitKey drops P frames after a lost video packet until a keyframe
	// arrives (see Depacketizer.TakeGap).
	waitKey := false
	var mux *ts.Muxer
	var reportedDroppedAudio int
	lastPing := time.Now()

	// On a connection of its own, a camera that goes quiet is caught by
	// the connection's idle timeout. A stream sharing a recorder's
	// connection has no such backstop: the other cameras keep the
	// connection busy while this one's messages stop altogether, so the
	// check after each message below would never run again. A ticker
	// checks it instead.
	var watch <-chan time.Time
	if sub != nil {
		t := time.NewTicker(min(mediaTimeout/4, time.Second))
		defer t.Stop()
		watch = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-watch:
			if time.Since(lastMedia) >= mediaTimeout {
				return fmt.Errorf("stream: %s: no video frame in %s: the recorder is sending other channels but not this one", cfg.Name, mediaTimeout)
			}
		case msg, open := <-src.Messages():
			if !open {
				if cErr := src.Err(); cErr != nil {
					return fmt.Errorf("stream: %s: camera closed the connection: %w", cfg.Name, cErr)
				}
				return fmt.Errorf("stream: %s: camera closed the connection", cfg.Name)
			}
			if privacyOn(msg) {
				return fmt.Errorf("stream: %s: %w", cfg.Name, ErrPrivacy)
			}
			if zoom && sub == nil && msg.Header.MsgID == baichuan.MsgIDVideo && byte(msg.Header.EncOffset) != zoomChannel {
				// The channel 0 sub stream that unlocks the zoom: wanted
				// only for that, so its media goes nowhere.
				continue
			}
			if len(msg.Payload) > 0 {
				d.Write(msg.Payload, msg.StartsPacket)
				if d.TakeGap() && !waitKey {
					waitKey = true
					log.Printf("stream: %s: camera cut a video frame short; skipping to the next keyframe (%d lost so far)", cfg.Name, d.Lost())
				}
				for {
					f, ok := d.Next()
					if !ok {
						break
					}
					if waitKey && f.Kind == baichuan.FramePFrame {
						// Refers to the lost picture: a decoder shown it
						// errors, and a hardware one can give up outright.
						continue
					}
					if f.Kind == baichuan.FrameIFrame {
						waitKey = false
					}
					switch f.Kind {
					case baichuan.FrameIFrame, baichuan.FramePFrame:
						// Only a decoded video frame resets the watchdog.
						// Audio deliberately does not: a camera can keep an
						// AAC track flowing on a stream whose video has
						// stalled or never started, since the two are
						// muxed independently once mux exists, and a
						// client asking for a video stream that is only
						// carrying audio is exactly as broken for this
						// project's purpose as one carrying nothing at
						// all. Resetting on audio would let that case hide
						// behind a watchdog that looks satisfied while
						// still delivering an unusable stream.
						lastMedia = time.Now()
						gotVideo = true
						if mux == nil {
							// The video codec is not known until the first
							// frame arrives, so the muxer is built here
							// rather than guessed at connect time.
							// NewMuxerWithAudio itself handles the wire's
							// uppercase codec spelling.
							//
							// Audio is always declared, whether or not this
							// camera turns out to send any: the recorder's
							// config, not a frame the stream has not seen
							// yet, is what says a stream should carry sound,
							// and this package has no view of that config.
							// A PMT that declares a silent audio track costs
							// nothing a client would notice; a stream that
							// only adds one after guessing right is exactly
							// how the previous tool lost audio without
							// anyone noticing for days. An AAC frame is
							// carried; anything else, ADPCM in particular,
							// is dropped and counted (see DroppedAudio
							// below), never relabeled.
							nm, err := ts.NewMuxerWithAudio(f.Codec, "aac")
							if err != nil {
								return fmt.Errorf("stream: %s: %w", cfg.Name, err)
							}
							mux = nm
							// Set the header before the first Publish. A client
							// that subscribes between Publish and SetHeader gets
							// no PAT/PMT and stalls until the next table repeat.
							h.SetHeader(mux.Header())
						}
						// FrameWithKey returns a fresh slice per call, so it
						// is safe to hand straight to PublishKey, which does
						// not copy it. The keyframe flag is what lets a
						// subscriber joining mid GOP wait for its own first
						// legal frame instead of a decoder choking on
						// slices with no parameter sets (see hub.PublishKey).
						// Only a stream that really is taller than wide
						// is turned when the camera asked: its other
						// resolutions and the sub stream come out upright.
						// A rotate setting applies as written. The hub
						// records it for reostream's own player, which
						// turns the picture itself because browsers ignore
						// the mark inside the video; that part works for
						// H.264 too, the mark only exists in H.265.
						if f.Kind == baichuan.FrameIFrame {
							tall := infoH > infoW
							if w, h, ok := pictureSize(f.Codec, f.Data); ok {
								tall = h > w
							}
							turned := rotate != 0 && (!rotateFromCamera || tall)
							if turned {
								h.SetRotate(rotate)
							} else {
								h.SetRotate(0)
							}
							if turned && isHEVC(f.Codec) {
								f.Data = withOrientation(f.Data, rotate)
								if !rotateLogged {
									log.Printf("stream: %s/%s: marking the picture to be shown turned %d degrees anticlockwise", cfg.Name, cfg.Stream, rotate)
									rotateLogged = true
								}
							}
						}
						// Before muxing, so an RTSP output receives the
						// frame rather than TS bytes.
						if cfg.Sink != nil {
							cfg.Sink.Frame(f)
						}
						if pkt, key := mux.FrameWithKey(f); pkt != nil {
							h.PublishKey(pkt, key)
							h.RecordFrame(len(pkt))
						}
						if f.Kind == baichuan.FrameIFrame {
							// Kept for the snapshot endpoint, which serves
							// this raw elementary stream directly rather
							// than decoding it: see hub.SetKeyframe.
							h.SetKeyframe(f.Codec, f.Video())
						}

					case baichuan.FrameInfo:
						if f.Width > 0 && f.Height > 0 {
							infoW, infoH = f.Width, f.Height
						}
						// Carries the picture size, which the muxer does not
						// need (the parameter sets say it) but a sink that
						// configures a hardware decoder before it has parsed
						// anything does. See frames.Stream.
						if cfg.Sink != nil {
							cfg.Sink.Frame(f)
						}

					case baichuan.FrameAAC, baichuan.FrameADPCM:
						if mux == nil {
							// No video frame yet, so no muxer exists to carry
							// this on and no codec to build one with.
							if droppedEarlyAudio(f.Kind) {
								h.AddDroppedAudio(1)
							}
							continue
						}
						if cfg.Sink != nil {
							cfg.Sink.Frame(f)
						}
						if pkt := mux.Frame(f); pkt != nil {
							h.Publish(pkt)
							h.AddAudioFrames(1)
						}
						if dropped := mux.DroppedAudio(); dropped > reportedDroppedAudio {
							h.AddDroppedAudio(dropped - reportedDroppedAudio)
							reportedDroppedAudio = dropped
						}
					}
				}
			}

			// Checked as an elapsed-time comparison after each message,
			// never as a select case beside conn.Messages(). Frames arrive
			// roughly every 40ms, so a timer case in that select would
			// almost never be the one chosen and the camera would time the
			// session out while this loop kept servicing frames.
			now := time.Now()
			if shouldPing(lastPing, now, pingInterval) {
				lastPing = now
				if err := src.Ping(); err != nil {
					return fmt.Errorf("stream: %s: ping: %w", cfg.Name, err)
				}
			}

			// Same elapsed-time-after-message pattern as the ping check
			// above, for the same reason: a timer case alongside
			// conn.Messages() in the select would almost never win against
			// a loop that is actually servicing frames every 40ms, so this
			// only needs to be checked when something on the connection
			// happens anyway. Checking only here, with no separate ticker,
			// is safe because a connection that stops producing messages
			// entirely is already bounded by baichuan's own idle-read
			// timeout (DefaultIdleTimeout, 15s), well under mediaTimeout:
			// this loop either sees another message within mediaTimeout, or
			// sees conn.Messages() close first because the idle timeout got
			// there sooner. See DefaultMediaTimeout for why the two do not
			// need to race each other.
			if now.Sub(lastMedia) >= mediaTimeout {
				if zoom && !gotVideo {
					useZoomOverRTSP(cfg.Name)
				}
				return fmt.Errorf("stream: %s: no video frame in %s: camera is answering but not sending media (held session)", cfg.Name, mediaTimeout)
			}
		}
	}
}

// shouldPing reports whether enough time has passed since the last ping.
// Extracted from Run so the elapsed-time decision can be tested directly
// without a real connection or a live camera clock.
func shouldPing(last, now time.Time, every time.Duration) bool {
	return now.Sub(last) >= every
}

// droppedEarlyAudio reports whether an audio frame that arrived before any
// video frame, and so before a muxer exists to carry it, should still count
// toward DroppedAudio. ADPCM is never carried regardless of when it arrives
// (see ts.NewMuxerWithAudio), so counting it here, rather than waiting for a
// muxer to exist first, is what keeps a camera whose audio leads its video,
// which is the ordering seen on every reconnect, from under-reporting
// DroppedAudio until the next video frame happens to land. AAC arriving
// this early is a genuine loss too, but not an unsupported-codec drop, so
// it does not count here.
func droppedEarlyAudio(kind baichuan.FrameKind) bool {
	return kind == baichuan.FrameADPCM
}

// zoomChannel is the TrackMix zoom lens's channel. The camera sends its
// video only on a connection that is also streaming channel 0 (asked for
// alone, channel 1 answers with a stream info frame and audio and nothing
// else), so a zoom stream opens channel 0's sub stream first, the cheapest
// one, and drops it. This is how the Reolink app asks (captured 2026-10-24):
// channel 0 sub on handle 256, channel 1 main on handle 1. The zoom's own
// sub stream is channel 1 sub on handle 257 (256 + channel, as for
// channel 0).
const zoomChannel = 1

// isZoom reports whether stream is one of the zoom lens's two streams.
func isZoom(stream string) bool {
	return stream == "zoom" || stream == "zoom_sub"
}

func startZoom(conn *baichuan.Conn, stream string) error {
	if err := conn.StartVideoOn(0, 256, baichuan.StreamSub); err != nil {
		return err
	}
	if stream == "zoom_sub" {
		return conn.StartVideoOn(zoomChannel, 256+zoomChannel, baichuan.StreamSub)
	}
	return conn.StartVideoOn(zoomChannel, zoomChannel, baichuan.StreamMain)
}

// zoomRTSP records cameras whose zoom never sent video over Baichuan, so
// later attempts use the camera's own RTSP instead (runNative) for the rest
// of this process's life.
var (
	zoomRTSPMu sync.Mutex
	zoomRTSP   = map[string]bool{}
)

func zoomOverRTSP(name string) bool {
	zoomRTSPMu.Lock()
	defer zoomRTSPMu.Unlock()
	return zoomRTSP[name]
}

func useZoomOverRTSP(name string) {
	zoomRTSPMu.Lock()
	zoomRTSP[name] = true
	zoomRTSPMu.Unlock()
	log.Printf("stream: %s: the zoom lens sent no video over Baichuan; using the camera's RTSP from now on", name)
}

// ErrPrivacy is a stream that cannot run because the camera is in privacy
// mode (E1 series): it turns down the preview with status 425, or reports
// sleep 1 on its own (sleep status report, 623) and stops sending video.
// Seen in a reocapture log of an E1 Pro (issue #13, 2026-10-25).
var ErrPrivacy = errors.New("the camera is in privacy mode")

// msgIDSleepStatus is the camera's own report of privacy mode turning on
// or off.
const msgIDSleepStatus = 623

// privacyOn reports whether msg says the camera has gone into privacy mode.
func privacyOn(msg baichuan.Message) bool {
	switch msg.Header.MsgID {
	case baichuan.MsgIDVideo:
		return msg.Header.Status() == 425 && len(msg.Payload) == 0
	case msgIDSleepStatus:
		return strings.Contains(strings.Join(strings.Fields(string(msg.XML)), ""), "<sleep>1</sleep>")
	}
	return false
}
