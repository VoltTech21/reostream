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
	"io"
	"net"
	"sync"
	"testing"
)

// Camera is a fake camera bound to a loopback address, for the lifetime of
// the test that created it.
type Camera struct {
	ln net.Listener

	mu     sync.Mutex
	closes int
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

func (c *Camera) acceptLoop(fixture []byte) {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			return
		}
		go c.serve(conn, fixture)
	}
}

func (c *Camera) serve(conn net.Conn, fixture []byte) {
	defer conn.Close()

	// A real Conn writes requests (the login, StartVideo, pings) that this
	// fake never inspects. Draining them keeps a real client from blocking
	// on a full socket buffer on a longer-running test; nothing here reads
	// them for content.
	drained := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
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
