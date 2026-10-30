package baichuan

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Shared runs many streams over one connection to an NVR or Home Hub.
//
// A recorder counts sessions, not streams: an RLN8-410 holds about ten
// connections and resets the eleventh and twelfth during login, so one
// connection per stream fits only half of an eight camera recorder. The
// Reolink app opens every channel it shows over a single connection, and
// the recorder allows it: measured on the RLN8-410 (firmware 3.5.1.368,
// 2026-10-28), ten sub stream previews on one connection all ran at the
// camera's full 15 fps, and channel 1's main and sub ran side by side at
// 25 and 15 fps.
//
// The recorder echoes each preview request's encryption offset on every
// media message it sends for it, so the offset's channel and stream bytes
// say which preview a message belongs to (see StartVideoOn). Two requests
// for the same channel and stream are both served, as two copies of the
// same picture told apart only by the request counter, so Shared asks for
// each channel and stream once and hands its messages to every reader of
// it: a TrackMix's main and zoom streams behind an NVR are the same
// channel 1 main stream, and are fetched once.
//
// Shared pings the connection itself; a reader's own Ping does nothing.
type Shared struct {
	conn *Conn
	sup  Support
	// supErr is why sup could not be read, if it could not.
	supErr error

	mu       sync.Mutex
	previews map[previewKey]*preview
	handle   int
	closed   bool
	err      error

	done chan struct{}

	// onIdle is told when the last preview closes, so whatever holds this
	// connection can decide whether to close it. See Pool.
	onIdle func(*Shared)
}

// previewKey is what a recorder's media message says about the preview it
// belongs to: the low two bytes of its encryption offset.
type previewKey struct{ channel, stream byte }

type preview struct {
	channel, handle int
	stream          string
	readers         map[*Sub]bool
	// info is the stream's last info packet. The recorder sends one when
	// a preview starts and not again, and it carries the picture size
	// that a sink configures a decoder with, so a reader that joins a
	// preview already running is handed this one first.
	info *Message
}

// sharedQueue is each reader's buffer, the same depth Conn gives its one
// reader.
const sharedQueue = 256

// stallTimeout is how long one reader may keep its buffer full before it is
// dropped. One goroutine hands every stream on the connection its
// messages, so a reader that stops reading would otherwise stall every
// other camera on the recorder with it. A healthy reader drains a full
// buffer in milliseconds; a var so a test can shorten it.
var stallTimeout = 2 * time.Second

// sharedPingInterval matches the cadence a stream pings its own connection
// at (internal/stream's pingInterval).
var sharedPingInterval = 10 * time.Second

// ErrSharedClosed is a subscription to a Shared whose connection has ended.
var ErrSharedClosed = errors.New("baichuan: shared connection closed")

// ErrReaderStalled ends a reader that stopped taking its messages; see
// stallTimeout.
var ErrReaderStalled = errors.New("baichuan: reader fell behind the shared connection and was dropped")

// NewShared takes over conn, which must have just been dialed and have
// nothing streaming yet. sup and supErr are the recorder's Support read,
// made before the connection is shared, since a read's reply cannot be told
// apart from any other once many readers share the connection.
func NewShared(conn *Conn, sup Support, supErr error) *Shared {
	s := &Shared{
		conn:     conn,
		sup:      sup,
		supErr:   supErr,
		previews: map[previewKey]*preview{},
		done:     make(chan struct{}),
	}
	go s.demux()
	go s.ping()
	return s
}

// DeviceInfo is the recorder's login reply (see Conn.DeviceInfo).
func (s *Shared) DeviceInfo() []byte { return s.conn.DeviceInfo() }

// Previews is how many distinct streams the connection is carrying.
func (s *Shared) Previews() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.previews)
}

// has reports whether the connection already carries channel's stream.
func (s *Shared) has(channel int, stream string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.previews[previewKey{byte(channel), StreamID(stream)}]
	return ok
}

// Closed reports whether the connection has ended.
func (s *Shared) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Done closes when the connection ends.
func (s *Shared) Done() <-chan struct{} { return s.done }

// Subscribe starts reading one channel's stream: StreamMain, StreamSub or
// StreamExtern. The stream is asked for only if no other reader on this
// connection already has it.
func (s *Shared) Subscribe(channel int, stream string) (*Sub, error) {
	key := previewKey{byte(channel), StreamID(stream)}
	sub := &Sub{s: s, key: key, msgs: make(chan Message, sharedQueue), quit: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSharedClosed
	}
	p := s.previews[key]
	if p == nil {
		// Handles only have to be distinct within the connection; the
		// stop message names the one its start used.
		s.handle++
		p = &preview{channel: channel, handle: s.handle, stream: stream, readers: map[*Sub]bool{}}
		if err := s.conn.StartVideoOn(channel, p.handle, stream); err != nil {
			return nil, fmt.Errorf("baichuan: start channel %d %s: %w", channel, stream, err)
		}
		s.previews[key] = p
		// The first reader sees the preview from its first message.
		sub.synced = true
	} else if p.info != nil {
		// The buffer is new and empty, and demux cannot see this reader
		// until the lock is released, so this cannot block.
		sub.msgs <- *p.info
	}
	p.readers[sub] = true
	return sub, nil
}

// unsubscribe removes sub, stopping its stream if nobody else reads it.
func (s *Shared) unsubscribe(sub *Sub) {
	s.mu.Lock()
	p := s.previews[sub.key]
	if p == nil || !p.readers[sub] {
		s.mu.Unlock()
		return
	}
	delete(p.readers, sub)
	if len(p.readers) == 0 {
		delete(s.previews, sub.key)
		if !s.closed {
			_ = s.conn.StopVideoOn(p.channel, p.handle, p.stream)
		}
	}
	idle := len(s.previews) == 0 && !s.closed
	s.mu.Unlock()
	if idle && s.onIdle != nil {
		s.onIdle(s)
	}
}

// closeIfIdle closes the connection if nothing is streaming on it, and
// reports whether it did.
func (s *Shared) closeIfIdle() bool {
	s.mu.Lock()
	if len(s.previews) > 0 || s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.conn.Close()
	return true
}

// Close ends the connection and every reader on it.
func (s *Shared) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}

// Err is why the connection ended, once it has.
func (s *Shared) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Shared) ping() {
	t := time.NewTicker(sharedPingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if s.conn.Ping() != nil {
				// The read side sees the same failure and ends the
				// connection; nothing more to do here.
				return
			}
		}
	}
}

// demux hands each media message to the readers of its preview, and ends
// every reader when the connection ends.
//
// Anything that is not a media message is dropped: the recorder pushes
// status documents for all its channels (floodlights, channel states) that
// no stream reads, and a config read's reply would need a reader that
// asked for it, which no stream on a shared connection does.
func (s *Shared) demux() {
	for m := range s.conn.Messages() {
		if m.Header.MsgID != MsgIDVideo {
			continue
		}
		key := previewKey{byte(m.Header.EncOffset), byte(m.Header.EncOffset >> 8)}
		s.mu.Lock()
		p := s.previews[key]
		if p == nil {
			// The tail of a stream just stopped.
			s.mu.Unlock()
			continue
		}
		isInfo := m.StartsPacket && (match(m.Payload, magicInfoV1) || match(m.Payload, magicInfoV2))
		if isInfo {
			mm := m
			p.info = &mm
		}
		keyframe := m.StartsPacket && match(m.Payload, magicIFrame)
		var to []*Sub
		for r := range p.readers {
			switch {
			case r.synced:
			case len(m.Payload) == 0 || isInfo:
				// A status reply, or the picture size: a reader still
				// waiting for a keyframe wants both.
			case keyframe:
				r.synced = true
			default:
				// A reader that joined partway through a picture
				// waits for the next keyframe, so it never starts on a
				// frame that refers to one it did not see.
				continue
			}
			to = append(to, r)
		}
		s.mu.Unlock()
		for _, r := range to {
			if !r.deliver(m) {
				s.drop(r)
			}
		}
	}

	err := s.conn.Err()
	if err == nil {
		err = ErrSharedClosed
	}
	s.mu.Lock()
	s.closed = true
	s.err = err
	var readers []*Sub
	for _, p := range s.previews {
		for r := range p.readers {
			readers = append(readers, r)
		}
	}
	s.previews = map[previewKey]*preview{}
	s.mu.Unlock()
	for _, r := range readers {
		r.end(err)
	}
	close(s.done)
}

// drop ends a reader that stalled, leaving the rest of the connection
// running.
func (s *Shared) drop(r *Sub) {
	r.end(ErrReaderStalled)
	s.unsubscribe(r)
}

// Sub is one reader's view of one stream on a Shared connection. It offers
// the part of Conn's surface a stream reads through.
type Sub struct {
	s   *Shared
	key previewKey

	msgs chan Message
	// quit closes when the reader closes the Sub, so a delivery waiting on
	// a full buffer gives up at once rather than after stallTimeout.
	quit     chan struct{}
	quitOnce sync.Once

	// synced is guarded by s.mu, and only demux reads or writes it once
	// the Sub is subscribed.
	synced bool

	// errMu guards err; msgs is closed only by demux, after err is set.
	errMu sync.Mutex
	err   error
}

// deliver hands m to the reader, waiting up to stallTimeout for room, and
// reports false if the reader stalled.
func (r *Sub) deliver(m Message) bool {
	select {
	case r.msgs <- m:
		return true
	case <-r.quit:
		return true
	default:
	}
	t := time.NewTimer(stallTimeout)
	defer t.Stop()
	select {
	case r.msgs <- m:
		return true
	case <-r.quit:
		return true
	case <-t.C:
		return false
	}
}

// end closes the reader's channel with err. Only demux calls it, which is
// what makes the close safe: demux is also the only sender.
func (r *Sub) end(err error) {
	r.errMu.Lock()
	if r.err != nil {
		r.errMu.Unlock()
		return
	}
	r.err = err
	r.errMu.Unlock()
	close(r.msgs)
}

// Messages yields this stream's media messages. It closes when the shared
// connection ends or this reader is dropped for falling behind; Err then
// says which.
func (r *Sub) Messages() <-chan Message { return r.msgs }

// Err reports why Messages closed.
func (r *Sub) Err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

// DeviceInfo is the recorder's login reply.
func (r *Sub) DeviceInfo() []byte { return r.s.DeviceInfo() }

// Support is the recorder's Support document, read once when the
// connection was opened.
func (r *Sub) Support() (Support, error) { return r.s.sup, r.s.supErr }

// Ping does nothing: the shared connection pings itself.
func (r *Sub) Ping() error { return nil }

// Close stops reading. The stream itself stops only when its last reader
// closes, and the connection stays up for the others.
func (r *Sub) Close() error {
	r.quitOnce.Do(func() { close(r.quit) })
	r.s.unsubscribe(r)
	return nil
}
