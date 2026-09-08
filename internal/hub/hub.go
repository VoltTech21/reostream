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

import "sync"

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

// Publish delivers b to every subscriber. It never blocks: a subscriber
// whose buffer is full is dropped instead of stalling this call, because
// this is called from the camera's read goroutine and any delay here is a
// delay in draining the socket.
//
// b is not copied per subscriber. The MPEG-TS muxer (internal/ts) allocates
// a fresh slice for every chunk it returns, so subscribers never see a
// buffer that gets mutated out from under them. A caller that reused buffers
// would need this to change.
func (h *Hub) Publish(b []byte) {
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
