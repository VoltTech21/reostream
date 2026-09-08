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
