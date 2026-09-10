package baichuan

import (
	"context"
	"fmt"
	"time"
)

// StreamInfo is what a camera's own media told us about one of its streams.
type StreamInfo struct {
	Codec  string
	Width  int
	Height int
}

// streamInfoTimeout bounds how long GetStreamInfo waits, after StartVideo,
// for a camera to send back its own Info packet and one coded frame. A
// healthy stream sends its Info packet immediately (see
// TestZZInfoFrameProbe-shaped evidence in h265_s2c.bin, where it is the very
// first media packet), so this is sized to absorb a slow network rather than
// tuned close to the common case, the same reasoning DefaultIdleTimeout
// documents for the streaming path.
const streamInfoTimeout = 8 * time.Second

// GetStreamInfo opens stream (StreamMain, StreamSub or StreamExtern) on its
// own connection and reads real media back long enough to learn the codec
// and resolution the camera is actually sending.
//
// This exists because no config message in ConfigMessages has ever been
// read against a captured reply and shown to carry a stream's resolution
// and codec: "enc" (message 56) is in the table as a read cmd/reocam can
// dump raw, but nothing has decoded its schema from a real camera, and
// docs/control.md's rule against sending a document nobody has seen applies
// to guessing a reply's shape as much as a request's. What is proven,
// because it is exactly how every number in docs/measurements.md's stream
// tables was established, is opening the stream itself and reading what the
// camera actually sends: the Info packet that starts it, and the first
// coded frame's own codec tag.
//
// It dials its own Conn rather than taking one that is already probing
// something else, and closes it before returning. Conn's own doc comment
// establishes that a camera permits one connection per stream; nothing
// establishes that calling StartVideo a second time on one connection, to
// switch which stream it is pulling, is safe, and a first-run setup probe
// is not the place to find that out against a real camera for the first
// time.
func GetStreamInfo(ctx context.Context, addr string, opts Options, stream string) (StreamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, streamInfoTimeout)
	defer cancel()

	conn, err := Dial(ctx, addr, opts)
	if err != nil {
		return StreamInfo{}, err
	}
	defer conn.Close()

	if err := conn.StartVideo(stream); err != nil {
		return StreamInfo{}, err
	}

	var info StreamInfo
	d := NewDepacketiser()
	for info.Codec == "" || (info.Width == 0 && info.Height == 0) {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				return streamInfoResult(info, stream, conn.Err())
			}
			if len(m.Payload) == 0 {
				continue
			}
			d.Write(m.Payload, m.StartsPacket)
			for {
				f, ok := d.Next()
				if !ok {
					break
				}
				switch f.Kind {
				case FrameInfo:
					info.Width, info.Height = f.Width, f.Height
				case FrameIFrame, FramePFrame:
					if f.Codec != "" {
						info.Codec = f.Codec
					}
				}
			}
		case <-ctx.Done():
			return streamInfoResult(info, stream, ctx.Err())
		}
	}
	return info, nil
}

// streamInfoResult decides what GetStreamInfo's wait ending early means. A
// codec with no resolution yet is still real information worth reporting
// (not every camera's Info packet has to arrive before its first frame);
// nothing at all means this stream never answered, which is reported
// against the stream name so a caller probing all three does not have to
// guess which one failed.
func streamInfoResult(info StreamInfo, stream string, cause error) (StreamInfo, error) {
	if info.Codec != "" {
		return info, nil
	}
	if cause == nil {
		cause = fmt.Errorf("no media arrived")
	}
	return StreamInfo{}, fmt.Errorf("baichuan: %s: %w", stream, cause)
}
