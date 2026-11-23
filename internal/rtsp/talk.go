package rtsp

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
)

// Talker opens a two-way audio session on the camera behind a stream.
type Talker interface {
	Open(ctx context.Context) (TalkSession, error)
}

// TalkSession is one open talk session. Write takes 16 bit mono PCM at rate.
type TalkSession interface {
	Write(rate int, pcm []int16) error
	Ping() error
	Close() error
}

// backChannelMedia is what a stream offers a client that asks for an ONVIF
// back channel. G.711 at 8 kHz, in both laws, comes first: go2rtc, and so
// Frigate's talk button, takes only the first format of a back channel and
// cannot send L16 (AlexxIT/go2rtc#696), so with L16 in front the mic button
// disappeared (#50, v0.2.4 to v0.2.9). 16 bit PCM at 16 kHz, the camera's
// own talk rate, follows for clients that choose a format themselves; it
// sounds better than going through 8 kHz (a tester's E1 Zoom report, 10-27).
func backChannelMedia() *description.Media {
	return &description.Media{
		Type:          description.MediaTypeAudio,
		IsBackChannel: true,
		Formats: []format.Format{
			&format.G711{PayloadTyp: 0, MULaw: true, SampleRate: 8000, ChannelCount: 1},
			&format.G711{PayloadTyp: 8, MULaw: false, SampleRate: 8000, ChannelCount: 1},
			&format.LPCM{PayloadTyp: 96, BitDepth: 16, SampleRate: 16000, ChannelCount: 1},
		},
	}
}

// talkAudio is one packet's worth of speech and the rate it was sent at.
type talkAudio struct {
	rate int
	pcm  []int16
}

// decodeL16 turns one RTP payload of 16 bit linear PCM, which RTP carries
// big-endian, into samples.
func decodeL16(payload []byte) []int16 {
	out := make([]int16, len(payload)/2)
	for i := range out {
		out[i] = int16(binary.BigEndian.Uint16(payload[i*2:]))
	}
	return out
}

// decodeG711 turns one RTP payload into PCM.
func decodeG711(f *format.G711, payload []byte) []int16 {
	var be []byte
	if f.MULaw {
		var m g711.Mulaw
		m.Unmarshal(payload)
		be = m
	} else {
		var a g711.Alaw
		a.Unmarshal(payload)
		be = a
	}
	out := make([]int16, len(be)/2)
	for i := range out {
		out[i] = int16(binary.BigEndian.Uint16(be[i*2:]))
	}
	return out
}

// talkIdle is how long a back channel may go quiet before its camera
// session is released. go2rtc keeps the RTSP session up for as long as
// anyone is watching, but only sends audio while someone holds the talk
// button, and a camera mid talk session is a camera nobody else can talk to.
const talkIdle = 3 * time.Second

// talkPing is how often an open session is pinged while waiting for more
// audio, well inside the connection's idle timeout.
const talkPing = 5 * time.Second

// backChannel carries one RTSP session's back channel audio to the camera.
// Packets arrive on gortsplib's goroutine, which must not block, so they are
// handed over a buffered channel to a goroutine of its own that owns the
// camera session: opening one takes a login, about a second, and the buffer
// holds the speech that arrives meanwhile.
type backChannel struct {
	name   string
	talker Talker
	audio  chan talkAudio
	stop   chan struct{}
	once   sync.Once
	done   chan struct{}
}

func newBackChannel(name string, t Talker) *backChannel {
	b := &backChannel{
		name:   name,
		talker: t,
		// 128 packets of 20 ms is about two and a half seconds.
		audio: make(chan talkAudio, 128),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go b.run()
	return b
}

// packet takes one RTP payload in format f. It never blocks; a backlog
// drops audio.
func (b *backChannel) packet(f format.Format, payload []byte) {
	var a talkAudio
	switch f := f.(type) {
	case *format.G711:
		a = talkAudio{rate: 8000, pcm: decodeG711(f, payload)}
	case *format.LPCM:
		if f.BitDepth != 16 || f.ChannelCount != 1 {
			return
		}
		a = talkAudio{rate: f.SampleRate, pcm: decodeL16(payload)}
	default:
		return
	}
	select {
	case b.audio <- a:
	default:
	}
}

func (b *backChannel) close() {
	b.once.Do(func() { close(b.stop) })
	<-b.done
}

func (b *backChannel) run() {
	defer close(b.done)
	var sess TalkSession
	release := func() {
		if sess != nil {
			if err := sess.Close(); err != nil {
				log.Printf("rtsp: %s: talk: close: %v", b.name, err)
			}
			sess = nil
		}
	}
	defer release()

	// After a failed open, wait before trying again rather than redialling
	// the camera for every 20 ms packet of a held button.
	var retryAt time.Time
	idle := time.NewTimer(talkIdle)
	defer idle.Stop()
	ping := time.NewTicker(talkPing)
	defer ping.Stop()

	for {
		select {
		case <-b.stop:
			return
		case a := <-b.audio:
			if sess == nil {
				if time.Now().Before(retryAt) {
					continue
				}
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					select {
					case <-b.stop:
						cancel()
					case <-ctx.Done():
					}
				}()
				s, err := b.talker.Open(ctx)
				cancel()
				if err != nil {
					log.Printf("rtsp: %s: talk: %v", b.name, err)
					retryAt = time.Now().Add(10 * time.Second)
					continue
				}
				log.Printf("rtsp: %s: talk started (%d Hz from the client)", b.name, a.rate)
				sess = s
			}
			if err := sess.Write(a.rate, a.pcm); err != nil {
				log.Printf("rtsp: %s: talk: %v", b.name, err)
				release()
				continue
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(talkIdle)
		case <-idle.C:
			if sess != nil {
				log.Printf("rtsp: %s: talk ended", b.name)
			}
			release()
			idle.Reset(talkIdle)
		case <-ping.C:
			if sess != nil {
				if err := sess.Ping(); err != nil && !errors.Is(err, context.Canceled) {
					release()
				}
			}
		}
	}
}
