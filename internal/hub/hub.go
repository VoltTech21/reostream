// Package hub broadcasts MPEG-TS chunks from one camera goroutine to any
// number of HTTP clients.
//
// It is the boundary that decides whether a slow client can hurt a camera.
// Neolink's per-consumer queues were unbounded, and a client that stopped
// reading (a stalled browser tab, a dead TCP connection the kernel had not
// noticed yet) grew forever until the process ran out of memory. This hub
// never lets that happen: a subscriber that cannot keep up is disconnected,
// not buffered.
package hub

import (
	"sync"
	"time"
)

// statsWindow is how long RecordFrame accumulates frames and bytes before
// turning them into a rate. A shorter window makes fps and bitrate jump
// around between individual frames arriving at slightly uneven intervals;
// 1 second smooths that out while still updating often enough that a status
// page reads as live rather than stale.
const statsWindow = 1 * time.Second

// FrameStats is a stream's throughput and health, as last reported by
// whatever is publishing to this hub.
//
// The hub does not compute any of this itself: it has no idea whether a
// published chunk is video or audio, or whether a frame just got dropped
// for an unsupported codec, only internal/stream does. Pushing the numbers
// up from there, rather than the hub or internal/server reaching down past
// it, is what keeps a hub a plain byte broadcaster and keeps HTTP concerns
// out of internal/stream.
type FrameStats struct {
	FPS          float64
	BitrateBps   float64
	LastFrameAt  time.Time
	AudioFrames  int
	DroppedAudio int
}

// Age reports how long ago the last frame was recorded, or zero if none
// ever was.
func (s FrameStats) Age() time.Duration {
	if s.LastFrameAt.IsZero() {
		return 0
	}
	return time.Since(s.LastFrameAt)
}

// subscriber pairs a channel with the small lock that makes closing it safe.
// Sending to and closing a channel from different goroutines is only safe if
// something serializes them; a close that races a send panics. That
// serialization has to be per-subscriber, not the hub-wide lock, because
// holding the hub-wide lock across a channel send would let one full
// subscriber stall Subscribe/Clients/Dropped for everyone else, which is the
// same kind of stall this package exists to prevent.
type subscriber struct {
	ch     chan []byte
	mu     sync.Mutex
	closed bool

	// synced is false until this subscriber has seen a video keyframe
	// boundary. A decoder handed slices before the parameter sets that
	// describe them logs reference errors until the next I frame (measured
	// against a live camera: about 9 error groups in the first 60 seconds
	// of a fresh join). Gating on the subscriber, not the hub, is what lets
	// a subscriber that joins after another one has already synced still
	// wait for its own keyframe: two subscribers joining at different
	// points in the GOP have different first legal frames.
	synced bool
}

// closeOnce closes the channel if it has not already been closed, reporting
// whether this call was the one that did it.
func (s *subscriber) closeOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	close(s.ch)
	return true
}

// Hub fans out frames to subscribers. The zero value is not usable; build
// one with New.
type Hub struct {
	buffer int

	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	dropped int
	header  []byte
	closed  bool

	statsMu     sync.Mutex
	stats       FrameStats
	windowStart time.Time
	windowVideo int
	windowBytes int
}

// New creates a Hub whose subscriber channels each hold up to buffer frames
// before being considered stalled.
func New(buffer int) *Hub {
	return &Hub{
		buffer: buffer,
		subs:   make(map[*subscriber]struct{}),
	}
}

// Subscribe registers a new listener and returns its channel along with a
// cancel function to unsubscribe. The cancel function is safe to call more
// than once, and safe to call concurrently with a Publish that is dropping
// the same subscriber: both paths call closeOnce, which only one of them
// wins.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	s := &subscriber{ch: make(chan []byte, h.buffer)}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		// Nothing will ever publish to this hub again; hand back an already
		// closed channel rather than one that would sit empty forever.
		close(s.ch)
		return s.ch, func() {}
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		if s.closeOnce() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
		}
	}
	return s.ch, cancel
}

// Close closes every current subscriber's channel and marks the hub closed,
// so a later Publish or Subscribe is a no-op rather than a send on, or a
// registration into, a hub nobody is going to deliver to again.
//
// Call this when the stream feeding the hub stops. Without it, an HTTP
// handler blocked in its read select (internal/server) only notices its
// stream is gone when its request context is cancelled, which on shutdown
// is the full 5 second timeout rather than an immediate close.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.subs = make(map[*subscriber]struct{})
	h.mu.Unlock()

	for _, s := range subs {
		s.closeOnce()
	}
}

// Publish delivers b to every synced subscriber, meaning "not a keyframe
// boundary": it is PublishKey(b, false). Every caller that has no notion of
// keyframes, audio in particular, calls this directly.
func (h *Hub) Publish(b []byte) {
	h.PublishKey(b, false)
}

// PublishKey delivers b to every subscriber, gated by whether that
// subscriber has synced to the stream yet. A subscriber that joins mid GOP
// must not receive anything until startsKeyframe is true for it: see
// subscriber.synced for why this is measured, not theoretical. Once a
// subscriber has synced, every later call, keyframe or not, delivers to it
// normally; startsKeyframe only matters for the transition.
//
// It never blocks: a subscriber whose buffer is full is dropped instead of
// stalling this call, because this is called from the camera's read
// goroutine and any delay here is a delay in draining the socket.
//
// b is not copied per subscriber. The MPEG-TS muxer (internal/ts) allocates
// a fresh slice for every chunk it returns, so subscribers never see a
// buffer that gets mutated out from under them. A caller that reused buffers
// would need this to change.
func (h *Hub) PublishKey(b []byte, startsKeyframe bool) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	// The send happens under only the per-subscriber lock, not the hub's.
	// One slow or dropped subscriber never delays the send to another.
	var dead []*subscriber
	for _, s := range subs {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		if !s.synced {
			if !startsKeyframe {
				// Nothing legal to hand this subscriber yet, video or
				// audio: buffering it would just be bytes it cannot use
				// once its decoder does start. This is a silent skip, not
				// the stalled-subscriber drop path below; the subscriber
				// is still healthy, just not synced yet.
				s.mu.Unlock()
				continue
			}
			s.synced = true
		}
		select {
		case s.ch <- b:
			s.mu.Unlock()
		default:
			s.closed = true
			close(s.ch)
			s.mu.Unlock()
			dead = append(dead, s)
		}
	}
	// Note: this inlines the same closed/close logic as subscriber.closeOnce
	// rather than calling it, because the select send must happen while
	// holding s.mu too, and closeOnce only covers the close path.

	if len(dead) > 0 {
		h.mu.Lock()
		for _, s := range dead {
			if _, present := h.subs[s]; present {
				delete(h.subs, s)
				h.dropped++
			}
		}
		h.mu.Unlock()
	}
}

// Clients returns the current subscriber count.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Dropped returns the total number of subscribers disconnected for falling
// behind, since the Hub was created.
func (h *Hub) Dropped() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// SetHeader records the MPEG-TS PAT/PMT pair a new subscriber should be sent
// before any live data. It is set once the muxer has seen the first frame
// and knows the codec; before that Header returns nil, and callers must not
// turn a nil header into an empty write.
//
// This is one snapshot, not a header rebuilt per joiner. Its PAT/PMT
// continuity counters are frozen at whatever they were the moment the caller
// built b, while the in-band PAT/PMT the muxer repeats every
// tableRepeatTicks keep advancing theirs (see ts.Muxer.Header). A joining
// client sees one continuity jump on PIDPAT and PIDPMT at join time, never
// again. ffmpeg does not check PSI continuity; a demuxer built strictly to
// the T-STD model can log it, but a table PID discontinuity carries no
// decode consequence the way one on PIDVideo would. Rebuilding it per joiner
// would mean the hub calling back into ts.Muxer for fresh counters, which
// does not fit the hub's job of only ever moving bytes it is handed; one
// cosmetic counter jump is cheaper than that coupling.
func (h *Hub) SetHeader(b []byte) {
	h.mu.Lock()
	h.header = b
	h.mu.Unlock()
}

// Header returns the current cached header, or nil if none has been set yet.
func (h *Hub) Header() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.header
}

// RecordFrame reports that n bytes of a published packet came from a video
// frame, for fps and bitrate. It is not the same call as Publish: a caller
// still calls Publish separately to actually deliver the bytes, since a
// muxer's PAT/PMT repeats and PCR-only packets also flow through Publish
// but are not a "frame" in any sense worth counting here.
//
// fps and bitrate are only ever computed from video: audio here is a
// constant-rate AAC track with no frame-rate concept worth reporting
// alongside it, and folding its bytes into "bitrate" would make the number
// jump depending on whether the camera happens to be sending audio this
// second, without saying why.
func (h *Hub) RecordFrame(n int) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	now := time.Now()
	h.stats.LastFrameAt = now
	if h.windowStart.IsZero() {
		h.windowStart = now
	}
	h.windowVideo++
	h.windowBytes += n
	if elapsed := now.Sub(h.windowStart); elapsed >= statsWindow {
		secs := elapsed.Seconds()
		h.stats.FPS = float64(h.windowVideo) / secs
		h.stats.BitrateBps = float64(h.windowBytes) * 8 / secs
		h.windowStart = now
		h.windowVideo = 0
		h.windowBytes = 0
	}
}

// AddDroppedAudio adds n to the running count of audio frames dropped for
// lacking a supported codec (see ts.Muxer.DroppedAudio). This total lives on
// the hub, not the muxer, because a muxer is rebuilt on every reconnect: a
// camera stuck sending ADPCM would otherwise look healthy again after every
// backoff cycle, resetting to 0 each time, which is exactly the kind of
// silent-audio condition this counter exists to catch.
func (h *Hub) AddDroppedAudio(n int) {
	if n == 0 {
		return
	}
	h.statsMu.Lock()
	h.stats.DroppedAudio += n
	h.statsMu.Unlock()
}

// AddAudioFrames adds n to the running count of audio frames actually
// muxed and published. Every stream now declares an audio track whether or
// not its camera ever sends one (see NewMuxerWithAudio), so DroppedAudio
// alone cannot tell "no audio arrives" from "audio is flowing fine": both
// read as 0. AudioFrames closes that gap. A camera with AudioFrames > 0 has
// working audio; one with AudioFrames == 0 and DroppedAudio == 0 sends no
// audio at all, which is a config or wiring question, not a codec one;
// DroppedAudio > 0 on its own means audio arrives but in a codec that gets
// discarded. All three are distinguishable only with both counters present.
func (h *Hub) AddAudioFrames(n int) {
	if n == 0 {
		return
	}
	h.statsMu.Lock()
	h.stats.AudioFrames += n
	h.statsMu.Unlock()
}

// MarkDisconnected zeroes the fps and bitrate gauges. Call it whenever the
// stream feeding this hub stops, whether for good or just for the next
// backoff cycle: without this, a camera that drops off mid-stream keeps
// reporting its last known rate on /api/status and /metrics forever,
// looking healthy to anyone glancing at fps or bitrate alone rather than
// cross-checking last_frame_age_seconds too. AudioFrames and DroppedAudio
// are untouched: those are cumulative health counters meant to survive a
// reconnect, not an instantaneous rate.
func (h *Hub) MarkDisconnected() {
	h.statsMu.Lock()
	h.stats.FPS = 0
	h.stats.BitrateBps = 0
	h.windowStart = time.Time{}
	h.windowVideo = 0
	h.windowBytes = 0
	h.statsMu.Unlock()
}

// Stats returns a snapshot of the hub's current frame statistics.
func (h *Hub) Stats() FrameStats {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	return h.stats
}
