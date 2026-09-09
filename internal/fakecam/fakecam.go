// Package fakecam fakes a Baichuan camera for tests, without a socket
// library of its own and without any protocol logic. It exists so
// internal/baichuan and internal/stream can each test against something
// that behaves like a camera on the wire, without ever dialling a real one.
//
// A committed capture (internal/baichuan/testdata) already contains a real
// login handshake, the camera's nonce reply followed by its login reply,
// immediately followed by an AES-encrypted media stream: exactly the byte
// sequence Conn expects to read from a socket. internal/baichuan's own tests
// already prove this by parsing a capture straight out of a byte slice.
// fakecam only moves the source of those same bytes onto a loopback
// listener; it does not re-implement or re-check anything about the
// protocol itself.
//
// The password a Conn dials fakecam with must match whatever password the
// capture was made under, or the AES key derived from the login nonce will
// not match the key the capture is encrypted with. Every fixture committed
// under internal/baichuan/testdata was captured against an empty password.
package fakecam

import (
	"net"
	"sync"
	"testing"
)

// Camera is a fake camera bound to a loopback address, for the lifetime of
// the test that created it.
type Camera struct {
	ln net.Listener

	mu       sync.Mutex
	closes   int
	accepts  int
	received []byte
}

// New starts a fake camera that replays fixture, verbatim, to every
// connection it accepts. It stops accepting when the test ends.
func New(t testing.TB, fixture []byte) *Camera {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakecam: listen: %v", err)
	}
	c := &Camera{ln: ln}
	go c.acceptLoop(fixture)
	t.Cleanup(func() { ln.Close() })
	return c
}

// Addr returns the address to dial, host:port.
func (c *Camera) Addr() string { return c.ln.Addr().String() }

// Closes reports how many served connections have run to completion: the
// fixture was fully written and the peer went on to close its side. Tests
// use it to confirm a client actually closed the connection rather than,
// say, exiting without doing so.
func (c *Camera) Closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// Accepts reports how many connections the camera has accepted. Tests use it
// to prove a client did not retry after a failure: a client holding a
// camera's one connection for a stream and quietly reconnecting looks, from
// here, like a second accept.
func (c *Camera) Accepts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepts
}

// Received returns every byte read from the camera's connections so far, in
// the order it arrived. It is meant for a single-connection test: with more
// than one connection the bytes are concatenated in whatever order the
// reader goroutines happened to run.
//
// Recording raw bytes rather than decoded messages keeps this package free
// of protocol logic (see the package doc). A caller that needs to know what
// a recorded byte range means, such as picking a client-sent message back
// out of it, decodes it with internal/baichuan's own Header type.
func (c *Camera) Received() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.received))
	copy(out, c.received)
	return out
}

func (c *Camera) recordAccept() {
	c.mu.Lock()
	c.accepts++
	c.mu.Unlock()
}

func (c *Camera) record(b []byte) {
	c.mu.Lock()
	c.received = append(c.received, b...)
	c.mu.Unlock()
}

func (c *Camera) acceptLoop(fixture []byte) {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		c.recordAccept()
		go c.serve(conn, fixture)
	}
}

// drain reads conn until it errors (typically EOF once the peer closes),
// recording every byte read. A real Conn writes requests (the login,
// StartVideo, pings, the stop message) that a plain replay never inspects;
// this is what lets a test recover them afterward without fakecam knowing
// what they mean.
func (c *Camera) drain(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			c.record(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (c *Camera) serve(conn net.Conn, fixture []byte) {
	defer conn.Close()

	// Draining on a separate goroutine keeps a real client from blocking on
	// a full socket buffer on a longer-running test.
	drained := make(chan struct{})
	go func() {
		c.drain(conn)
		close(drained)
	}()

	// Ignore the write error: a test that cancels early closes conn from the
	// client side before the whole fixture goes out, which is expected, not
	// a fakecam failure.
	conn.Write(fixture)

	<-drained
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
}

// NewWriter starts a fake camera whose connections are driven entirely by
// write, called once per accepted connection with the socket and a channel
// that closes once the client side of that connection has gone away (its
// reads have hit EOF or an error). fakecam only accepts, drains, and counts;
// write decides what bytes go out and when.
//
// It exists for a scenario none of this file's other constructors can
// express: a session that must keep answering on the wire indefinitely,
// past whatever a single static byte slice contains, without ever sending
// media again. New, NewDropAfter and NewPartial are all built around
// writing a fixed byte slice once; a held camera session that keeps a
// client's connection alive with periodic protocol traffic for as long as
// the test needs it to has no fixed length to give them. Only a live
// callback can keep going for exactly as long as the caller decides.
//
// This still does not put protocol logic in fakecam. write is supplied by
// the caller (see internal/stream's own tests, which build wire bytes with
// internal/baichuan's exported Header type, the same way
// internal/baichuan's own fakecam_test.go builds synthetic messages that no
// committed capture contains); this constructor only ever calls it.
func NewWriter(t testing.TB, write func(conn net.Conn, clientGone <-chan struct{})) *Camera {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakecam: listen: %v", err)
	}
	c := &Camera{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.recordAccept()
			go func(conn net.Conn) {
				defer conn.Close()
				drained := make(chan struct{})
				go func() {
					c.drain(conn)
					close(drained)
				}()
				write(conn, drained)
				<-drained
				c.mu.Lock()
				c.closes++
				c.mu.Unlock()
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

// NewDropAfter starts a fake camera that writes only fixture[:n] to each
// connection it accepts and then stops sending, without waiting for the
// peer to finish reading.
//
// It reproduces a camera vanishing mid-stream: n is chosen to land after a
// few real messages have gone out, so a client has something flowing before
// the connection disappears out from under it. The half of the connection
// this closes is the write side only (TCP FIN, via CloseWrite), not a full
// Close: closing a socket outright while the kernel is still holding bytes
// the peer hasn't read yet sends RST instead of a clean EOF, which would
// make the client see the wrong error (a mid-write reset while sending an
// unrelated request) instead of the one this test wants, a clean read
// failure on Conn's own read side.
func NewDropAfter(t testing.TB, fixture []byte, n int) *Camera {
	t.Helper()
	if n > len(fixture) {
		n = len(fixture)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakecam: listen: %v", err)
	}
	c := &Camera{ln: ln}
	prefix := fixture[:n]
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.recordAccept()
			go func(conn net.Conn) {
				go c.drain(conn)
				conn.Write(prefix)
				if tc, ok := conn.(*net.TCPConn); ok {
					tc.CloseWrite()
				} else {
					conn.Close()
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}

// NewPartial starts a fake camera that writes only fixture[:n] to each
// connection it accepts and then goes silent: no more data, and no close,
// for as long as the connection stays open.
//
// It reproduces a server that finishes login and then holds the session
// open in silence, exactly what a stuck camera session looks like from the
// client side: nothing on the wire ever tells the client something is
// wrong.
func NewPartial(t testing.TB, fixture []byte, n int) *Camera {
	t.Helper()
	if n > len(fixture) {
		n = len(fixture)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakecam: listen: %v", err)
	}
	c := &Camera{ln: ln}
	prefix := fixture[:n]
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.recordAccept()
			go func(conn net.Conn) {
				go c.drain(conn)
				conn.Write(prefix)
				// Deliberately never closes conn or writes again: ln.Close
				// in Cleanup is what ends this connection, same as an
				// operator killing a wedged camera process.
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return c
}
