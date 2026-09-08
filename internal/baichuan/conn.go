package baichuan

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Options configures a connection.
type Options struct {
	Username string
	Password string
	Channel  int
}

// Conn is one Baichuan session carrying one stream.
//
// A camera permits one connection per stream — main, sub and extern are
// independent — so a Conn must be closed cleanly. Close sends the stream-stop
// message that releases the camera's session; a process killed without doing
// so leaves that stream refusing connections for minutes.
type Conn struct {
	nc     net.Conn
	r      *Reader
	w      *Writer
	opts   Options
	nonce  string
	stream string
	handle int

	counter byte
	mu      sync.Mutex

	msgs      chan Message
	closeOnce sync.Once
}

// Dial connects to a camera and logs in. addr may omit the port, in which
// case 9000 is used.
func Dial(ctx context.Context, addr string, opts Options) (*Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "9000")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("baichuan: dial %s: %w", addr, err)
	}
	c, err := newConn(ctx, nc, opts)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func newConn(ctx context.Context, nc net.Conn, opts Options) (*Conn, error) {
	c := &Conn{
		nc:   nc,
		r:    NewReader(nc),
		w:    NewWriter(nc),
		opts: opts,
		msgs: make(chan Message, 256),
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	if err := c.login(); err != nil {
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

// nextCounter returns the per-request counter carried in the encryption offset.
func (c *Conn) nextCounter() byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counter++
	return c.counter
}

// login performs the handshake observed on the wire: a zero-length
// negotiation probe, the camera's nonce, a hashed login, then DeviceInfo.
// Encryption switches to AES once login completes.
func (c *Conn) login() error {
	probe := Header{
		MsgID:   MsgIDLogin,
		Class:   ClassLegacy,
		EncByte: NegotiateByte,
		DirByte: DirRequest,
	}
	if err := c.w.Write(probe, nil); err != nil {
		return fmt.Errorf("baichuan: send negotiation probe: %w", err)
	}
	m, err := c.r.Next()
	if err != nil {
		return fmt.Errorf("baichuan: read negotiation reply: %w", err)
	}
	nonce, err := parseNonce(m.XML)
	if err != nil {
		return err
	}
	c.nonce = nonce

	body, err := loginXML(c.opts.Username, c.opts.Password, nonce)
	if err != nil {
		return err
	}
	if err := c.w.Write(Header{MsgID: MsgIDLogin, Class: ClassModern24}, body); err != nil {
		return fmt.Errorf("baichuan: send login: %w", err)
	}
	reply, err := c.r.Next()
	if err != nil {
		return fmt.Errorf("baichuan: read login reply: %w", err)
	}
	if len(reply.XML) == 0 {
		return fmt.Errorf("baichuan: empty login reply")
	}

	key := AESKey(nonce, c.opts.Password)
	c.r.SetAESKey(key)
	c.w.SetAESKey(key)
	return nil
}

// Nonce reports the login nonce.
func (c *Conn) Nonce() string { return c.nonce }

func (c *Conn) readLoop() {
	defer close(c.msgs)
	for {
		m, err := c.r.Next()
		if err != nil {
			return
		}
		c.msgs <- m
	}
}

// Messages yields every message the camera sends after login.
func (c *Conn) Messages() <-chan Message { return c.msgs }

// StartVideo requests a stream: StreamMain, StreamSub or StreamExtern.
func (c *Conn) StartVideo(stream string) error {
	c.handle++
	c.stream = stream
	body, err := previewXML(c.opts.Channel, c.handle, stream)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     MsgIDVideo,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), StreamID(stream), c.nextCounter(), 0),
	}
	return c.w.Write(h, body)
}

// Ping keeps the session alive. The camera times out a session it stops
// hearing from, logging "session:%u login timeout", and the official NVR
// heartbeats continuously.
func (c *Conn) Ping() error {
	h := Header{
		MsgID:     MsgIDPing,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(0, 0, c.nextCounter(), 0),
	}
	return c.w.Write(h, nil)
}

// Close stops the stream and releases the camera's session.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.stream != "" {
			if body, mErr := previewXML(c.opts.Channel, c.handle, ""); mErr == nil {
				h := Header{
					MsgID: MsgIDVideo,
					Class: ClassModern24,
					EncOffset: EncOffsetFor(byte(c.opts.Channel),
						StreamID(c.stream), c.nextCounter(), 0),
				}
				_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_ = c.w.Write(h, body)
			}
		}
		err = c.nc.Close()
	})
	return err
}
