// Package stream joins one camera to one hub: dial, depacketise, mux,
// publish, on repeat until the context is cancelled or something fails.
package stream

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
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

type Config struct {
	Name     string
	Address  string
	Username string
	Password string
	Stream   string // "main", "sub" or "extern"

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
// MPEG-TS to h until ctx is cancelled or an error occurs. It never retries:
// retry and backoff belong to the supervisor that runs many of these, since
// a runner that retries internally would hold the camera's one connection
// for that stream and block its own replacement from ever connecting.
//
// Run returns promptly on an already cancelled context without dialling, so
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

	kind, ok := streamKind(cfg.Stream)
	if !ok {
		return fmt.Errorf("stream: %s: unknown stream %q", cfg.Name, cfg.Stream)
	}

	conn, err := baichuan.Dial(ctx, cfg.Address, baichuan.Options{
		Username: cfg.Username,
		Password: cfg.Password,
	})
	if err != nil {
		return fmt.Errorf("stream: %s: dial: %w", cfg.Name, err)
	}
	// Close unconditionally on every return path. Close sends the
	// stream-stop message that releases the camera's session; skipping it
	// leaves the camera refusing new connections on this stream for minutes.
	defer conn.Close()

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

	if err := conn.StartVideo(kind); err != nil {
		return fmt.Errorf("stream: %s: start video: %w", cfg.Name, err)
	}

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
	lastMedia := time.Now()

	d := baichuan.NewDepacketiser()
	var mux *ts.Muxer
	var reportedDroppedAudio int
	lastPing := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, open := <-conn.Messages():
			if !open {
				if cErr := conn.Err(); cErr != nil {
					return fmt.Errorf("stream: %s: camera closed the connection: %w", cfg.Name, cErr)
				}
				return fmt.Errorf("stream: %s: camera closed the connection", cfg.Name)
			}
			if len(msg.Payload) > 0 {
				d.Write(msg.Payload, msg.StartsPacket)
				for {
					f, ok := d.Next()
					if !ok {
						break
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
							// below), never relabelled.
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
				if err := conn.Ping(); err != nil {
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
