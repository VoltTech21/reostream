// Package talk plays audio from an RTSP client through a camera's speaker.
//
// The camera side is internal/baichuan's: OpenTalk negotiates the session and
// Talk carries the blocks. What this package adds is the conversion between
// the two ends. An RTSP back channel arrives as G.711 at 8 kHz in packets of
// whatever size the client chose; the cameras surveyed here take IMA ADPCM at
// 16 kHz in blocks of exactly 1024 samples. So audio is resampled to the
// camera's rate, collected until a whole block is there, and sent a block at
// a time.
package talk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/adpcm"
	"github.com/VoltTech21/reostream/internal/baichuan"
)

// Dialer opens a fresh connection to one camera.
type Dialer func(ctx context.Context) (*baichuan.Conn, error)

// openTimeout bounds login plus negotiation. A person pressing the talk
// button is waiting on it, so failing in seconds beats a long hang.
const openTimeout = 10 * time.Second

// Session is one talk session on its own camera connection, separate from
// the ones carrying video, so talking never risks a stream.
type Session struct {
	mu      sync.Mutex
	conn    *baichuan.Conn
	rate    int
	enc     adpcm.Encoder
	pending []int16
	rs      resampler
	lim     limiter
	gain    float64 // linear; 0 until SetGain, read as 1
	closed  bool
}

// SetGain sets the talk gain in dB (config talk_gain_db) for what Write
// sends from now on.
func (s *Session) SetGain(db int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gain = math.Pow(10, float64(db)/20)
}

// Open dials the camera and negotiates a talk session.
func Open(ctx context.Context, dial Dialer) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("talk: dial: %w", err)
	}
	f, err := baichuan.OpenTalk(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("talk: %w", err)
	}
	if f.SampleRate <= 0 {
		conn.Close()
		return nil, fmt.Errorf("talk: camera offered sample rate %d", f.SampleRate)
	}
	return &Session{conn: conn, rate: f.SampleRate}, nil
}

// ErrClosed is Write after Close.
var ErrClosed = errors.New("talk: session closed")

// Write plays pcm, 16 bit mono at inRate, through the camera. Samples short
// of a whole block wait for the next call.
func (s *Session) Write(inRate int, pcm []int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	gain := s.gain
	if gain == 0 {
		gain = 1
	}
	pcm = s.lim.apply(pcm, gain, inRate)
	s.pending = s.rs.convert(s.pending, pcm, inRate, s.rate)
	for len(s.pending) >= adpcm.SamplesPerBlock {
		block := s.enc.EncodeBlock(s.pending[:adpcm.SamplesPerBlock])
		s.pending = s.pending[adpcm.SamplesPerBlock:]
		if err := s.conn.Talk(baichuan.TalkPacket(block)); err != nil {
			return fmt.Errorf("talk: send: %w", err)
		}
	}
	return nil
}

// Ping keeps the connection alive between bursts of speech. The camera
// sends nothing back during a talk session, and a connection that hears
// nothing for baichuan.DefaultIdleTimeout gives up on itself.
func (s *Session) Ping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.conn.Ping()
}

// Close sends what is left, padded to a block, and releases the camera.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if len(s.pending) > 0 {
		_ = s.conn.Talk(baichuan.TalkPacket(s.enc.EncodeBlock(s.pending)))
		s.pending = nil
	}
	// "talk close" is the message the Reolink app sends to end a session,
	// and the app waits for the camera's answer before it hangs up.
	// Hanging up at once left an RLC-833A refusing every later talk
	// session with 422 until it rebooted (#18), so wait for the answer,
	// briefly, before closing.
	if err := s.conn.RawChannelRequest(baichuan.MsgIDTalkClose); err == nil {
		waitFor(s.conn.Messages(), baichuan.MsgIDTalkClose, talkCloseWait)
	}
	return s.conn.Close()
}

// talkCloseWait bounds the wait for a camera's answer to talk close.
var talkCloseWait = 2 * time.Second

// waitFor reads msgs until one with id arrives, the channel closes, or d
// passes, dropping anything else (the camera's own talk audio, keepalives).
func waitFor(msgs <-chan baichuan.Message, id uint32, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case m, ok := <-msgs:
			if !ok || m.Header.MsgID == id {
				return
			}
		case <-t.C:
			return
		}
	}
}

// limitCeiling is the limiter's ceiling, -1 dBFS.
var limitCeiling = 32767 * math.Pow(10, -1.0/20)

// limitRelease is how quickly the limiter lets go after a peak.
const limitRelease = 50 * time.Millisecond

// limiter applies a gain and holds peaks at limitCeiling: it turns down at
// once when a sample would go over, and back up over limitRelease. Full
// scale overdrove an E1 Zoom's speaker (a tester, 10-29), and a gain on top
// of a loud voice would otherwise clip.
type limiter struct {
	env float64 // the current reduction, 1 for none; 0 before the first call
}

// apply returns pcm, at rate, times gain, limited. pcm itself is left alone.
func (l *limiter) apply(pcm []int16, gain float64, rate int) []int16 {
	if l.env == 0 {
		l.env = 1
	}
	if rate <= 0 {
		rate = 16000
	}
	release := 1 - math.Exp(-1/(limitRelease.Seconds()*float64(rate)))
	out := make([]int16, len(pcm))
	for i, x := range pcm {
		v := float64(x) * gain
		if a := math.Abs(v) * l.env; a > limitCeiling {
			l.env = limitCeiling / math.Abs(v)
		}
		out[i] = int16(math.Round(v * l.env))
		l.env += (1 - l.env) * release
	}
	return out
}

// resampler converts between sample rates by linear interpolation, carrying
// its position across calls so packet boundaries do not click. Speech
// through a doorbell speaker does not need better than this.
type resampler struct {
	pos  float64 // position of the next output sample, in input samples, relative to last
	last int16   // the final sample of the previous call
	init bool
}

// convert appends in, at inRate, to out at outRate.
func (r *resampler) convert(out, in []int16, inRate, outRate int) []int16 {
	if len(in) == 0 {
		return out
	}
	if inRate == outRate || inRate <= 0 {
		return append(out, in...)
	}
	if !r.init {
		r.last, r.init = in[0], true
	}
	step := float64(inRate) / float64(outRate)
	// Sample i of the virtual input is last for i == 0 and in[i-1] after it,
	// so interpolation can bridge from the previous call into this one.
	at := func(i int) float64 {
		if i == 0 {
			return float64(r.last)
		}
		return float64(in[i-1])
	}
	n := len(in)
	for r.pos < float64(n) {
		i := int(r.pos)
		frac := r.pos - float64(i)
		v := at(i)*(1-frac) + at(i+1)*frac
		out = append(out, int16(v))
		r.pos += step
	}
	r.pos -= float64(n)
	r.last = in[n-1]
	return out
}
