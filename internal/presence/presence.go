// Package presence keeps a battery camera present to a recorder without
// waking it. Its stream is made only from what reostream already has: the
// last picture, repeated while nothing happens; each motion clip the
// camera uploads, played in at real speed as it arrives; and the live
// picture whenever someone has woken the camera to watch it.
//
// A recorder such as Frigate can then hold the stream open around the
// clock. Repeated identical pictures are no motion to it, and a clip is
// real motion, so its own detection runs on the footage.
//
// The stream always has a sound track, AAC-LC at 16 kHz mono, which is
// what these cameras record: a clip's own audio while it plays, silence
// the rest of the time. See audioLocked.
package presence

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/mp4read"
)

// Sink receives the stream's frames: the camera's RTSP output.
type Sink interface {
	Frame(baichuan.Frame)
}

var (
	// idleEvery is how often the still is repeated.
	idleEvery = time.Second
	// liveGrace is how long after the last live or clip frame the still
	// takes over again.
	liveGrace = 1500 * time.Millisecond
	// primeFrames is how many copies of the first still are sent at once,
	// so the RTSP output has a run of pictures (and the sound beside them)
	// to describe the stream from straight away.
	primeFrames = 32
)

// The sound track. RTSP describes a stream once, and a reader holds it on
// that description, so the track has to be there from the first still and
// stay: a stream that grew one when a clip arrived would have to drop every
// reader to say so. And a track that is declared but carries nothing
// between clips stalls players that wait for both (mpegts.js did, for the
// TrackMix zoom stream), so the stills carry silence.
//
// The track's format is fixed for the same reason: AAC-LC, 16 kHz, mono,
// what every camera seen so far records. A clip whose audio is anything
// else plays with silence.
const (
	audioRate        = 16000
	audioFrameMicros = 1024 * 1_000_000 / audioRate // 64 ms
)

// silence is one frame of that track with nothing in it, ADTS framed: a
// single channel element with no scale factor bands and so no spectral
// data, then the end element.
var silence = []byte{0xff, 0xf1, 0x60, 0x40, 0x01, 0x7f, 0xfc, 0x00, 0xc8, 0x00, 0x07}

// Player is one battery camera's presence stream.
type Player struct {
	camera string
	sink   Sink

	mu       sync.Mutex
	codec    string
	still    []byte
	clock    uint32 // the stream's own clock, microseconds
	active   time.Time
	primed   bool
	liveLast uint32
	liveSeen bool
	playing  bool

	// audioAt is where the next sound frame starts on the stream's clock,
	// once audioOn; clip is the audio of the clip being played, if any;
	// shown is when the last picture went out, which paces the sound
	// between pictures.
	audioAt uint32
	audioOn bool
	clip    *clipAudio
	shown   time.Time

	clips chan string
}

// clipAudio is a clip's sound, read in full before it plays.
type clipAudio struct {
	frames [][]byte // ADTS
	at     []int64  // when each starts, microseconds after the first picture
	base   uint32   // the stream's clock at the clip's first picture
	next   int
}

// New builds the player for camera, writing to sink.
func New(camera string, sink Sink) *Player {
	return &Player{camera: camera, sink: sink, clips: make(chan string, 8)}
}

var (
	regMu    sync.Mutex
	registry = map[string]*Player{}
)

// Register makes p the player for its camera.
func Register(p *Player) {
	regMu.Lock()
	registry[p.camera] = p
	regMu.Unlock()
}

// Lookup returns camera's player, or nil.
func Lookup(camera string) *Player {
	regMu.Lock()
	defer regMu.Unlock()
	return registry[camera]
}

// Run repeats the still and plays queued clips until ctx ends.
func (p *Player) Run(ctx context.Context) {
	tick := time.NewTicker(idleEvery)
	defer tick.Stop()
	sound := time.NewTicker(audioFrameMicros * time.Microsecond)
	defer sound.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-p.clips:
			p.play(ctx, path)
		case <-tick.C:
			p.mu.Lock()
			if p.still != nil && time.Since(p.active) >= liveGrace {
				p.emitLocked(baichuan.FrameIFrame, p.codec, p.still, uint32(idleEvery/time.Microsecond))
			}
			p.mu.Unlock()
		case <-sound.C:
			// Between pictures the sound goes out as time passes rather
			// than a second at a time with each still: a reader that
			// stamps packets as they arrive (ffmpeg with
			// -use_wallclock_as_timestamps) or that starts each track's
			// clock at its first packet (ffmpeg over RTSP, until a sender
			// report) would otherwise misplace a burst; measured, ffmpeg
			// wrote a second of silence at one timestamp. It never runs
			// further ahead of the last picture than the next still will.
			p.mu.Lock()
			if p.audioOn {
				ahead := min(time.Since(p.shown), idleEvery)
				p.audioLocked(p.clock + uint32(ahead/time.Microsecond))
			}
			p.mu.Unlock()
		}
	}
}

// Frame passes a live frame through (the camera is awake because someone
// is watching it), on the stream's own clock. It is the sink the camera's
// on-demand main stream writes to.
func (p *Player) Frame(f baichuan.Frame) {
	if f.Kind != baichuan.FrameIFrame && f.Kind != baichuan.FramePFrame {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.playing {
		return // a clip has the stream
	}
	dur := uint32(40_000)
	if p.liveSeen {
		if d := f.Micros - p.liveLast; d > 0 && d < 2_000_000 {
			dur = d
		}
	}
	p.liveLast, p.liveSeen = f.Micros, true
	data := f.Video()
	if f.Kind == baichuan.FrameIFrame && p.adoptLocked(f.Codec, data) {
		p.primeLocked()
	}
	p.emitLocked(f.Kind, f.Codec, data, dur)
	p.active = time.Now()
}

// Seed takes the still from a clip already on disk, without playing it:
// the picture to show until the next clip or live view.
func (p *Player) Seed(path string) {
	t, err := mp4read.Open(path)
	if err != nil {
		return
	}
	defer t.Close()
	k := t.LastKey()
	if k < 0 {
		return
	}
	data, err := t.AnnexB(k)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.adoptLocked(t.Codec, data) {
		p.primeLocked()
	}
}

// SeedFrame takes the still from a keyframe reostream kept from the last
// time the camera was awake.
func (p *Player) SeedFrame(codec string, es []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.adoptLocked(codec, es) {
		p.primeLocked()
	}
}

// Play queues a newly uploaded clip to be played into the stream.
func (p *Player) Play(path string) {
	select {
	case p.clips <- path:
	default:
		log.Printf("presence: %s: too many clips waiting, skipping %s", p.camera, path)
	}
}

func (p *Player) play(ctx context.Context, path string) {
	t, err := mp4read.Open(path)
	if err != nil {
		log.Printf("presence: %s: %v", p.camera, err)
		return
	}
	defer t.Close()
	p.mu.Lock()
	if p.codec != "" && t.Codec != p.codec {
		p.mu.Unlock()
		log.Printf("presence: %s: clip is %s but the stream is %s; not played", p.camera, t.Codec, p.codec)
		p.Seed(path)
		return
	}
	p.playing = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.playing = false
		p.mu.Unlock()
	}()

	scale := t.Timescale
	if scale == 0 {
		scale = 90_000
	}
	// vt is the clip's own time of each picture, in its timescale; last
	// the time of the one played before, in microseconds.
	vt := t.Start
	var last int64
	var sound *clipAudio
	started := false
	for i, s := range t.Samples {
		at := vt * 1_000_000 / int64(scale)
		vt += int64(s.Dur)
		if !started && !s.Key {
			continue // a decoder starts at a keyframe
		}
		first := !started
		if first {
			// The sound is lined up with the first picture played.
			sound = p.clipAudio(t, at)
		}
		started = true
		data, err := t.AnnexB(i)
		if err != nil {
			log.Printf("presence: %s: %v", p.camera, err)
			return
		}
		// The picture shows for dur, and is stamped the clip's own time
		// since the one before: the same thing while the samples are all
		// as long, but not where one is stretched (over a gap between
		// fragments, or an encoder's priming), and then the sound would
		// drift from the picture.
		dur := uint32(uint64(s.Dur) * 1_000_000 / uint64(scale))
		stamp := uint32(at - last)
		last = at
		kind := baichuan.FramePFrame
		if s.Key {
			kind = baichuan.FrameIFrame
		}
		p.mu.Lock()
		if s.Key && p.adoptLocked(t.Codec, data) {
			p.primeLocked()
		}
		if first {
			stamp = dur
			// The sound may have gone on past the last still (see Run);
			// the clip starts where it has got to, so none of the
			// clip's sound comes too late to play.
			if ahead := p.audioAt - p.clock; p.audioOn && int32(ahead) > int32(stamp) {
				stamp = ahead
			}
			if sound != nil {
				// Where emitLocked is about to stamp this picture.
				sound.base = p.clock + stamp
			}
			p.clip = sound
		}
		p.emitLocked(kind, t.Codec, data, stamp)
		p.active = time.Now()
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(dur) * time.Microsecond):
		}
	}
	log.Printf("presence: %s: played %s", p.camera, path)
}

// clipAudio reads t's sound, timed from first, the clip's own time of its
// first picture played in microseconds, or returns nil when there is none
// the stream can carry.
func (p *Player) clipAudio(t *mp4read.Track, first int64) *clipAudio {
	a := t.Audio
	if a == nil || a.Timescale == 0 {
		return nil
	}
	if a.Object != 2 || a.Rate != audioRate || a.Channels != 1 {
		log.Printf("presence: %s: clip audio is %d Hz, %d channels (object type %d), not the stream's 16 kHz mono AAC-LC; it plays silent",
			p.camera, a.Rate, a.Channels, a.Object)
		return nil
	}
	ca := &clipAudio{}
	at := a.Start
	for i, s := range a.Samples {
		data, err := a.ADTS(i)
		if err != nil {
			log.Printf("presence: %s: clip audio: %v", p.camera, err)
			return nil
		}
		ca.frames = append(ca.frames, data)
		ca.at = append(ca.at, at*1_000_000/int64(a.Timescale)-first)
		at += int64(s.Dur)
	}
	return ca
}

// audioLocked sends the sound up to until, on the stream's clock, a frame
// at a time and back to back: the clip's own frame for each slot if a clip
// is playing and has one there, silence otherwise. A clip's frame goes in
// the slot nearest its own time, so it plays within half a frame (32 ms)
// of its picture. Each frame states its place in Micros, on the same clock
// as the video (see rtsp.Stream.audioRTPTime).
func (p *Player) audioLocked(until uint32) {
	if !p.audioOn {
		p.audioAt, p.audioOn = p.clock, true
	}
	const half = audioFrameMicros / 2
	for int32(until-p.audioAt) >= 0 {
		data := silence
		if c := p.clip; c != nil {
			rel := int64(int32(p.audioAt - c.base))
			for c.next < len(c.at) && c.at[c.next] < rel-half {
				c.next++ // its slot has gone by
			}
			if c.next < len(c.at) && c.at[c.next] < rel+half {
				data = c.frames[c.next]
				c.next++
			}
			if c.next == len(c.at) {
				p.clip = nil
			}
		}
		p.sink.Frame(baichuan.Frame{Kind: baichuan.FrameAAC, Data: data, Micros: p.audioAt})
		p.audioAt += audioFrameMicros
	}
}

// adoptLocked takes data as the still. It reports whether this is the
// stream's first picture, which fixes its codec.
func (p *Player) adoptLocked(codec string, data []byte) bool {
	if p.codec != "" && codec != p.codec {
		return false
	}
	first := p.codec == ""
	p.codec = codec
	p.still = append([]byte(nil), data...)
	return first
}

func (p *Player) primeLocked() {
	if p.primed || p.still == nil {
		return
	}
	p.primed = true
	for i := 0; i < primeFrames; i++ {
		p.emitLocked(baichuan.FrameIFrame, p.codec, p.still, 33_000)
	}
}

func (p *Player) emitLocked(kind baichuan.FrameKind, codec string, data []byte, dur uint32) {
	if codec != p.codec {
		return
	}
	p.clock += dur
	p.sink.Frame(baichuan.Frame{Kind: kind, Codec: codec, Data: data, Micros: p.clock})
	p.shown = time.Now()
	// The sound catches up with the picture: a still or a clip's first
	// picture can be stamped ahead of where it had got to.
	p.audioLocked(p.clock)
}
