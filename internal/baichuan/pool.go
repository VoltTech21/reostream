package baichuan

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Pool hands out streams from NVRs and Home Hubs over a few shared
// connections (see Shared), and plain connections everywhere else.
//
// Whether an address is a recorder is learned from its own login reply,
// not configured: a camera on its own keeps one connection per stream,
// which is how every camera here has run since the first release and
// which nothing about a camera's session limit asks to change.
type Pool struct {
	// MaxPreviews caps the streams one shared connection carries before
	// the next one opens. A recorder has room for about ten connections,
	// so a handful of streams on each keeps an eight camera recorder's
	// main and sub streams to four connections, while no single
	// connection has to carry every camera's 4K main stream at once.
	MaxPreviews int

	// dial is Dial, replaced in tests.
	dial func(ctx context.Context, addr string, opts Options) (*Conn, error)

	mu     sync.Mutex
	groups map[string]*poolGroup
}

// DefaultMaxPreviews is Pool.MaxPreviews when it is zero.
const DefaultMaxPreviews = 4

// supportTimeout bounds the Support read a new shared connection makes.
var supportTimeout = 5 * time.Second

type poolGroup struct {
	conns []*Shared
	// dialing is closed when a dial in progress for this address ends, so
	// streams starting together wait for one login instead of each
	// opening a connection of their own: the very thing the pool exists
	// to prevent.
	dialing chan struct{}
	// camera records that the address answered as a camera on its own the
	// last time it was dialed, so its streams dial in parallel as before.
	camera bool
}

// NewPool returns an empty Pool.
func NewPool() *Pool {
	return &Pool{dial: Dial, groups: map[string]*poolGroup{}}
}

// poolKey is one login at one address. Streams reached with different
// credentials never share a connection.
func poolKey(addr string, opts Options) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "9000")
	}
	return strings.ToLower(addr) + "\x00" + opts.Username + "\x00" + opts.Password
}

// IsRecorder reports whether a login reply came from an NVR or Home Hub:
// every camera has "ipc" in its type at login, and a recorder does not.
func IsRecorder(deviceInfo []byte) bool {
	di, err := ParseDeviceInfo(deviceInfo)
	if err != nil {
		return false
	}
	t := strings.ToLower(di.DeviceInfo.Type + " " + di.DeviceInfo.TypeInfo)
	return strings.TrimSpace(t) != "" && !strings.Contains(t, "ipc")
}

// Open starts channel's stream at addr. From a recorder it returns a
// reader on a shared connection; from a camera on its own, or a battery
// camera (opts.UID), it returns that camera's own logged-in connection
// with nothing started, exactly as Dial would, for the caller to run and
// close as before. Exactly one of the two is non-nil when err is nil.
func (p *Pool) Open(ctx context.Context, addr string, opts Options, channel int, stream string) (*Sub, *Conn, error) {
	if opts.UID != "" {
		c, err := p.dial(ctx, addr, opts)
		return nil, c, err
	}
	max := p.MaxPreviews
	if max <= 0 {
		max = DefaultMaxPreviews
	}
	key := poolKey(addr, opts)
	for {
		p.mu.Lock()
		g := p.groups[key]
		if g == nil {
			g = &poolGroup{}
			p.groups[key] = g
		}
		if g.camera {
			p.mu.Unlock()
			return p.openCamera(ctx, key, addr, opts)
		}
		live := g.conns[:0]
		for _, s := range g.conns {
			if !s.Closed() {
				live = append(live, s)
			}
		}
		g.conns = live
		// A stream another reader already has costs the recorder nothing
		// more, wherever it is; a new one goes on the first connection
		// with room.
		var pick *Shared
		for _, s := range g.conns {
			if s.has(channel, stream) {
				pick = s
				break
			}
		}
		if pick == nil {
			for _, s := range g.conns {
				if s.Previews() < max {
					pick = s
					break
				}
			}
		}
		if pick != nil {
			sub, err := pick.Subscribe(channel, stream)
			p.mu.Unlock()
			if err == ErrSharedClosed {
				continue
			}
			return sub, nil, err
		}
		if wait := g.dialing; wait != nil {
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		g.dialing = make(chan struct{})
		p.mu.Unlock()

		sub, conn, err := p.openNew(ctx, g, addr, opts, channel, stream)
		p.mu.Lock()
		close(g.dialing)
		g.dialing = nil
		p.mu.Unlock()
		return sub, conn, err
	}
}

// openNew dials addr for g and, if it is a recorder, shares the connection
// and subscribes to it.
func (p *Pool) openNew(ctx context.Context, g *poolGroup, addr string, opts Options, channel int, stream string) (*Sub, *Conn, error) {
	conn, err := p.dial(ctx, addr, opts)
	if err != nil {
		return nil, nil, err
	}
	if !IsRecorder(conn.DeviceInfo()) {
		p.mu.Lock()
		g.camera = true
		p.mu.Unlock()
		return nil, conn, nil
	}
	sctx, cancel := context.WithTimeout(ctx, supportTimeout)
	sup, supErr := GetSupport(sctx, conn)
	cancel()
	s := NewShared(conn, sup, supErr)
	s.onIdle = p.idle
	// Subscribed before the connection is published to the group, so a
	// connection this call opened is never left in the pool with nothing
	// on it and nobody to close it.
	sub, err := s.Subscribe(channel, stream)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	p.mu.Lock()
	g.conns = append(g.conns, s)
	p.mu.Unlock()
	return sub, nil, nil
}

// openCamera dials a camera on its own, and puts its address back to
// sharing if it now answers as a recorder: an address whose device was
// swapped is told apart by its next login, not by a restart.
func (p *Pool) openCamera(ctx context.Context, key, addr string, opts Options) (*Sub, *Conn, error) {
	conn, err := p.dial(ctx, addr, opts)
	if err != nil {
		return nil, nil, err
	}
	if IsRecorder(conn.DeviceInfo()) {
		p.mu.Lock()
		if g := p.groups[key]; g != nil {
			g.camera = false
		}
		p.mu.Unlock()
	}
	return nil, conn, nil
}

// idle closes a shared connection whose last stream has stopped, and drops
// it from its group.
func (p *Pool) idle(s *Shared) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !s.closeIfIdle() {
		return
	}
	for _, g := range p.groups {
		for i, c := range g.conns {
			if c == s {
				g.conns = append(g.conns[:i:i], g.conns[i+1:]...)
				return
			}
		}
	}
}

// Connections is how many shared connections the pool holds open.
func (p *Pool) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, g := range p.groups {
		for _, s := range g.conns {
			if !s.Closed() {
				n++
			}
		}
	}
	return n
}
