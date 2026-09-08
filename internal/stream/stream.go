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

// Config names one camera stream to run.
type Config struct {
	Name     string
	Address  string
	Username string
	Password string
	Stream   string // "main", "sub" or "extern"
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

	d := baichuan.NewDepacketiser()
	var mux *ts.Muxer
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
					if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
						continue
					}
					if mux == nil {
						// The codec is not known until the first frame
						// arrives, so the muxer is built here rather than
						// guessed at connect time. NewMuxer itself handles
						// the wire's uppercase spelling.
						nm, err := ts.NewMuxer(f.Codec)
						if err != nil {
							return fmt.Errorf("stream: %s: %w", cfg.Name, err)
						}
						mux = nm
						// Set the header before the first Publish. A client
						// that subscribes between Publish and SetHeader gets
						// no PAT/PMT and stalls until the next table repeat.
						h.SetHeader(mux.Header())
					}
					// Frame returns a fresh slice per call, so it is safe to
					// hand straight to Publish, which does not copy it.
					if pkt := mux.Frame(f); pkt != nil {
						h.Publish(pkt)
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
		}
	}
}

// shouldPing reports whether enough time has passed since the last ping.
// Extracted from Run so the elapsed-time decision can be tested directly
// without a real connection or a live camera clock.
func shouldPing(last, now time.Time, every time.Duration) bool {
	return now.Sub(last) >= every
}
