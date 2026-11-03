package bcudp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"
)

// Ports a battery camera listens on for C2D_C.
var connectPorts = []int{2018, 2015}

const (
	resendEvery    = 500 * time.Millisecond
	ackEvery       = 15 * time.Millisecond
	heartbeatEvery = time.Second
	silenceLimit   = 30 * time.Second

	// defaultConnectTimeout applies when Dial's context has no deadline.
	defaultConnectTimeout = 20 * time.Second

	// maxAhead bounds how far past a gap packets are kept.
	maxAhead = 4096
)

// ErrSilent means the camera sent nothing for silenceLimit.
var ErrSilent = errors.New("bcudp: camera stopped answering")

// Conn is one UDP session with a battery camera, as a reliable ordered byte
// stream. It implements net.Conn.
type Conn struct {
	sock   *net.UDPConn
	remote *net.UDPAddr
	// reply is the shared socket on the reply port, when one is set: sock
	// is then reply's, and this session only borrows it. See replyport.go.
	reply    *replySock
	cid, did uint32
	// ip is the address dialled, noted as ours until Close. See Ours.
	ip net.IP

	mu        sync.Mutex
	nextID    uint32
	unacked   map[uint32]*outPacket
	expect    uint32
	gotAny    bool
	ahead     map[uint32][]byte
	buf       []byte
	err       error
	closed    bool
	readDL    time.Time
	lastHeard time.Time
	wake      chan struct{}

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type outPacket struct {
	b    []byte
	sent time.Time
}

// Dial wakes the battery camera at host, opens a session with it, and
// returns the session as a byte stream carrying Baichuan.
func Dial(ctx context.Context, host, uid string) (*Conn, error) {
	return dial(ctx, host, uid, connectPorts)
}

func dial(ctx context.Context, host, uid string, ports []int) (*Conn, error) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	rport, err := replyPort()
	if err != nil {
		return nil, err
	}
	sock, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("bcudp: listen: %w", err)
	}
	var c *Conn
	oursOpen(ip)
	if rport == 0 {
		c, err = connect(ctx, sock, ip, uid, ports)
		if err != nil {
			sock.Close()
		}
	} else {
		// sock only carries C2D_C; the session is on the reply port.
		c, err = connectOnReplyPort(ctx, sock, rport, ip, uid, ports)
		sock.Close()
	}
	if err != nil {
		oursClosed(ip)
		return nil, err
	}
	c.ip = ip
	return c, nil
}

func resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("bcudp: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return ips[0], nil
}

func connect(ctx context.Context, sock *net.UDPConn, ip net.IP, uid string, ports []int) (*Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultConnectTimeout)
	}
	cid := newCID()
	port := sock.LocalAddr().(*net.UDPAddr).Port
	pkt := encodeDiscovery(newTID(), connectXML(uid, port, cid, ""))

	buf := make([]byte, 65536)
	for {
		for _, p := range ports {
			_, _ = sock.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: p})
		}
		next := time.Now().Add(resendEvery)
		for time.Now().Before(next) {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("bcudp: connect %s: %w", ip, err)
			}
			if !time.Now().Before(deadline) {
				return nil, fmt.Errorf("bcudp: connect %s: no answer for uid %s: %w", ip, uid, os.ErrDeadlineExceeded)
			}
			dl := next
			if deadline.Before(dl) {
				dl = deadline
			}
			_ = sock.SetReadDeadline(dl)
			n, from, err := sock.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				return nil, fmt.Errorf("bcudp: connect %s: %w", ip, err)
			}
			if !from.IP.Equal(ip) || magicOf(buf[:n]) != magicDiscovery {
				continue
			}
			_, body, err := decodeDiscovery(buf[:n])
			if err != nil {
				continue
			}
			m, err := parseP2P(body)
			if err != nil || m.ConnectR == nil || m.ConnectR.CID != cid {
				continue
			}
			if m.ConnectR.Rsp != 0 {
				return nil, fmt.Errorf("bcudp: connect %s: camera refused (rsp %d)", ip, m.ConnectR.Rsp)
			}
			_ = sock.SetReadDeadline(time.Time{})
			return newConn(sock, from, cid, m.ConnectR.DID), nil
		}
	}
}

// connectOnReplyPort is connect with the camera told to answer on the reply
// port: C2D_C goes out from sock, and the session runs on the shared
// socket.
func connectOnReplyPort(ctx context.Context, sock *net.UDPConn, rport int, ip net.IP, uid string, ports []int) (*Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultConnectTimeout)
	}
	r, err := acquireReply(rport)
	if err != nil {
		return nil, err
	}
	type reply struct {
		from *net.UDPAddr
		rep  *connectRep
	}
	got := make(chan reply, 1)
	cid := newCID()
	r.onCID(cid, func(b []byte, from *net.UDPAddr) {
		if magicOf(b) != magicDiscovery {
			return
		}
		_, body, err := decodeDiscovery(b)
		if err != nil {
			return
		}
		m, err := parseP2P(body)
		if err != nil || m.ConnectR == nil {
			return
		}
		select {
		case got <- reply{from, m.ConnectR}:
		default:
		}
	})
	pkt := encodeDiscovery(newTID(), connectXML(uid, rport, cid, ""))
	t := time.NewTimer(0)
	defer t.Stop()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			r.dropCID(cid)
			r.release()
			return nil, fmt.Errorf("bcudp: connect %s: %w", ip, ctx.Err())
		case <-timeout.C:
			r.dropCID(cid)
			r.release()
			return nil, fmt.Errorf("bcudp: connect %s: no answer for uid %s on reply port %d: %w", ip, uid, rport, os.ErrDeadlineExceeded)
		case <-t.C:
			for _, p := range ports {
				_, _ = sock.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: p})
			}
			t.Reset(resendEvery)
		case g := <-got:
			if g.rep.Rsp != 0 {
				r.dropCID(cid)
				r.release()
				return nil, fmt.Errorf("bcudp: connect %s: camera refused (rsp %d)", ip, g.rep.Rsp)
			}
			// Through a port forward the reply is from the forward, not
			// the camera, and the session is sent back the same way.
			c := newSession(r.sock, g.from, cid, g.rep.DID)
			c.reply = r
			r.onCID(cid, func(b []byte, from *net.UDPAddr) {
				if from.IP.Equal(c.remote.IP) {
					c.handle(b)
				}
			})
			c.start()
			return c, nil
		}
	}
}

func newConn(sock *net.UDPConn, remote *net.UDPAddr, cid, did uint32) *Conn {
	c := newSession(sock, remote, cid, did)
	c.wg.Add(1)
	go c.recvLoop()
	c.start()
	return c
}

func newSession(sock *net.UDPConn, remote *net.UDPAddr, cid, did uint32) *Conn {
	return &Conn{
		sock:      sock,
		remote:    remote,
		cid:       cid,
		did:       did,
		unacked:   make(map[uint32]*outPacket),
		ahead:     make(map[uint32][]byte),
		lastHeard: time.Now(),
		wake:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// start runs the session's timers: acks, resends, heartbeats.
func (c *Conn) start() {
	c.wg.Add(1)
	go c.timerLoop()
}

// signalLocked wakes every Read waiting for data.
func (c *Conn) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func (c *Conn) failLocked(err error) {
	if c.err == nil {
		c.err = err
	}
	c.signalLocked()
}

func (c *Conn) recvLoop() {
	defer c.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, from, err := c.sock.ReadFromUDP(buf)
		if err != nil {
			c.mu.Lock()
			c.failLocked(err)
			c.mu.Unlock()
			return
		}
		if from.IP.Equal(c.remote.IP) {
			c.handle(buf[:n])
		}
	}
}

func (c *Conn) handle(b []byte) {
	switch magicOf(b) {
	case magicData:
		p, err := decodeData(b)
		if err != nil || p.conn != c.cid {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lastHeard = time.Now()
		c.gotAny = true
		d := p.id - c.expect
		switch {
		case int32(d) < 0:
		case d == 0:
			c.buf = append(c.buf, p.payload...)
			c.expect++
			for {
				next, ok := c.ahead[c.expect]
				if !ok {
					break
				}
				delete(c.ahead, c.expect)
				c.buf = append(c.buf, next...)
				c.expect++
			}
			c.signalLocked()
		case d < maxAhead:
			if _, ok := c.ahead[p.id]; !ok {
				c.ahead[p.id] = append([]byte(nil), p.payload...)
			}
		}
	case magicAck:
		a, err := decodeAck(b)
		if err != nil || a.conn != c.cid {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lastHeard = time.Now()
		base := a.last + 1
		for id := range c.unacked {
			if int32(id-base) < 0 {
				delete(c.unacked, id)
			}
		}
		for i, h := range a.have {
			if h == 1 {
				delete(c.unacked, base+uint32(i))
			}
		}
	case magicDiscovery:
		_, body, err := decodeDiscovery(b)
		if err != nil {
			return
		}
		m, err := parseP2P(body)
		if err != nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lastHeard = time.Now()
		if m.DDisc != nil && (m.DDisc.CID == c.cid || m.DDisc.DID == c.did) {
			c.failLocked(io.EOF)
		}
	}
}

// ackLocked describes what has arrived so far.
func (c *Conn) ackLocked() []byte {
	a := ackPacket{conn: c.did, last: c.expect - 1}
	if !c.gotAny {
		a.group = none
	}
	if len(c.ahead) > 0 {
		var top uint32
		for id := range c.ahead {
			if d := id - c.expect; d > top {
				top = d
			}
		}
		a.have = make([]byte, top+1)
		for i := range a.have {
			if _, ok := c.ahead[c.expect+uint32(i)]; ok {
				a.have[i] = 1
			}
		}
	}
	return encodeAck(a)
}

func (c *Conn) timerLoop() {
	defer c.wg.Done()
	t := time.NewTicker(ackEvery)
	defer t.Stop()
	var lastHB time.Time
	for {
		var now time.Time
		select {
		case <-c.done:
			return
		case now = <-t.C:
		}
		c.mu.Lock()
		if c.err != nil {
			c.mu.Unlock()
			return
		}
		out := [][]byte{c.ackLocked()}
		for _, p := range c.unacked {
			if now.Sub(p.sent) >= resendEvery {
				p.sent = now
				out = append(out, p.b)
			}
		}
		if now.Sub(c.lastHeard) > silenceLimit {
			c.failLocked(ErrSilent)
		}
		c.mu.Unlock()
		if now.Sub(lastHB) >= heartbeatEvery {
			lastHB = now
			out = append(out, encodeDiscovery(newTID(), sessionXML("C2D_HB", c.cid, c.did)))
		}
		for _, b := range out {
			_, _ = c.sock.WriteToUDP(b, c.remote)
		}
	}
}

// Read reads the camera's byte stream. A D2C_DISC from the camera reads as
// io.EOF once everything before it has been read.
func (c *Conn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.buf) > 0 && !c.closed {
			n := copy(b, c.buf)
			c.buf = c.buf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return 0, err
		}
		dl, wake := c.readDL, c.wake
		c.mu.Unlock()

		var expired <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			t := time.NewTimer(d)
			expired = t.C
			select {
			case <-wake:
				t.Stop()
				continue
			case <-expired:
				return 0, os.ErrDeadlineExceeded
			}
		}
		<-wake
	}
}

// Write queues b for the camera, split into packets, and sends them. It does
// not wait for acks.
func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if c.err != nil && c.err != io.EOF {
		return 0, c.err
	}
	now := time.Now()
	for off := 0; off < len(b); off += maxPayload {
		end := min(off+maxPayload, len(b))
		p := encodeData(c.did, c.nextID, b[off:end])
		c.unacked[c.nextID] = &outPacket{b: p, sent: now}
		c.nextID++
		if _, err := c.sock.WriteToUDP(p, c.remote); err != nil {
			return off, err
		}
	}
	return len(b), nil
}

// Close ends the session, telling the camera so it can go back to sleep.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.failLocked(net.ErrClosed)
		c.mu.Unlock()
		close(c.done)
		disc := encodeDiscovery(newTID(), sessionXML("C2D_DISC", c.cid, c.did))
		for i := 0; i < 3; i++ {
			if i > 0 {
				time.Sleep(20 * time.Millisecond)
			}
			_, _ = c.sock.WriteToUDP(disc, c.remote)
		}
		if c.reply != nil {
			c.reply.dropCID(c.cid)
			c.reply.release()
		} else {
			err = c.sock.Close()
		}
		c.wg.Wait()
		if c.ip != nil {
			oursClosed(c.ip)
		}
	})
	return err
}

func (c *Conn) LocalAddr() net.Addr  { return c.sock.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

func (c *Conn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDL = t
	c.signalLocked()
	c.mu.Unlock()
	return nil
}

// SetWriteDeadline is accepted and ignored: Write never blocks on the camera.
func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

func newTID() uint32 { return rand.Uint32() }

func newCID() uint32 { return rand.Uint32()&0x7fffffff | 1 }
