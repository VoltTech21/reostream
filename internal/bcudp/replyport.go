package bcudp

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
)

// Both a search and a connect name the port the camera is to answer on,
// and by default that is the sending socket's own port. Behind NAT that
// rewrites source ports (Docker Desktop on a Mac, where even host
// networking is the VM's) the camera answers a port nothing forwards. With
// a reply port set, every search and session names that one port instead,
// and reostream listens on it; Docker publishes it with
// -p <port>:<port>/udp.
//
// One socket on that port serves every search and session at once. A
// reply that came in through Docker's port forward is from Docker's
// gateway, not the camera, so replies are told apart by what they carry:
// a search reply echoes the search's tid, and every session packet the
// camera sends names our cid.
//
// The search and the connect go out from a socket of their own: through
// Docker Desktop, the camera never answered one sent from the published
// port (tested 2026-11-02). The session after it is sent from the reply
// port, to the address its connect reply came from. Through Docker that is
// the port forward's own address for the camera's session port, which
// relays it on, and the camera then sees the session come from the port it
// was told to answer.

// ReplyPortEnv names the environment variable that sets the reply port
// when SetReplyPort has not.
const ReplyPortEnv = "REOSTREAM_BATTERY_UDP_PORT"

var replyPortSet struct {
	mu   sync.Mutex
	port int
	set  bool
}

// SetReplyPort makes every later search and session use port for the
// camera's replies; 0 goes back to each socket's own port.
func SetReplyPort(port int) {
	replyPortSet.mu.Lock()
	defer replyPortSet.mu.Unlock()
	replyPortSet.port, replyPortSet.set = port, true
}

// replyPort is the port set, from SetReplyPort or else the environment,
// or 0 for none.
func replyPort() (int, error) {
	replyPortSet.mu.Lock()
	port, set := replyPortSet.port, replyPortSet.set
	replyPortSet.mu.Unlock()
	if set {
		return port, nil
	}
	v := os.Getenv(ReplyPortEnv)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("bcudp: %s=%q is not a port", ReplyPortEnv, v)
	}
	return n, nil
}

// replySock is the shared socket on one reply port.
type replySock struct {
	sock *net.UDPConn
	port int
	refs int

	// Read by the receive loop; guarded by replySocks.mu.
	byTID map[uint32]func(b []byte, from *net.UDPAddr)
	byCID map[uint32]func(b []byte, from *net.UDPAddr)
}

// replySocks holds the open reply sockets by port, so a port changed by
// SetReplyPort while sessions are open leaves those sessions alone.
var replySocks struct {
	mu sync.Mutex
	m  map[int]*replySock
}

// acquireReply opens the socket on port, or takes another reference to
// the one already open. Each acquire is paired with a release.
func acquireReply(port int) (*replySock, error) {
	replySocks.mu.Lock()
	defer replySocks.mu.Unlock()
	if r := replySocks.m[port]; r != nil {
		r.refs++
		return r, nil
	}
	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("bcudp: listen on reply port %d: %w", port, err)
	}
	r := &replySock{
		sock:  sock,
		port:  port,
		refs:  1,
		byTID: map[uint32]func([]byte, *net.UDPAddr){},
		byCID: map[uint32]func([]byte, *net.UDPAddr){},
	}
	if replySocks.m == nil {
		replySocks.m = map[int]*replySock{}
	}
	replySocks.m[port] = r
	go r.recvLoop()
	return r, nil
}

// release drops a reference, closing the socket with the last one.
func (r *replySock) release() {
	replySocks.mu.Lock()
	defer replySocks.mu.Unlock()
	r.refs--
	if r.refs > 0 {
		return
	}
	if replySocks.m[r.port] == r {
		delete(replySocks.m, r.port)
	}
	r.sock.Close()
}

// onTID and onCID route packets for a search's tid or a session's cid to
// fn, until the matching drop call. fn runs on the receive loop and must
// copy b to keep it.
func (r *replySock) onTID(tid uint32, fn func([]byte, *net.UDPAddr)) {
	replySocks.mu.Lock()
	r.byTID[tid] = fn
	replySocks.mu.Unlock()
}

func (r *replySock) onCID(cid uint32, fn func([]byte, *net.UDPAddr)) {
	replySocks.mu.Lock()
	r.byCID[cid] = fn
	replySocks.mu.Unlock()
}

func (r *replySock) dropTID(tid uint32) {
	replySocks.mu.Lock()
	delete(r.byTID, tid)
	replySocks.mu.Unlock()
}

func (r *replySock) dropCID(cid uint32) {
	replySocks.mu.Lock()
	delete(r.byCID, cid)
	replySocks.mu.Unlock()
}

func (r *replySock) recvLoop() {
	buf := make([]byte, 65536)
	for {
		n, from, err := r.sock.ReadFromUDP(buf)
		if err != nil {
			// Closed by the last release, or broken: then the sessions
			// on it go silent and fail as such, and the next dial opens
			// a new socket.
			replySocks.mu.Lock()
			if replySocks.m[r.port] == r {
				delete(replySocks.m, r.port)
			}
			replySocks.mu.Unlock()
			return
		}
		if fn := r.route(buf[:n]); fn != nil {
			fn(buf[:n], from)
		}
	}
}

// route finds who a packet is for: data and acks name the cid, a search
// reply echoes the search's tid, and the camera's other messages name the
// cid in their XML.
func (r *replySock) route(b []byte) func([]byte, *net.UDPAddr) {
	var cid uint32
	switch magicOf(b) {
	case magicData:
		p, err := decodeData(b)
		if err != nil {
			return nil
		}
		cid = p.conn
	case magicAck:
		a, err := decodeAck(b)
		if err != nil {
			return nil
		}
		cid = a.conn
	case magicDiscovery:
		tid, body, err := decodeDiscovery(b)
		if err != nil {
			return nil
		}
		replySocks.mu.Lock()
		fn := r.byTID[tid]
		replySocks.mu.Unlock()
		if fn != nil {
			return fn
		}
		m, err := parseP2P(body)
		if err != nil {
			return nil
		}
		switch {
		case m.ConnectR != nil:
			cid = m.ConnectR.CID
		case m.DHB != nil:
			cid = m.DHB.CID
		case m.DDisc != nil:
			cid = m.DDisc.CID
		default:
			return nil
		}
	default:
		return nil
	}
	replySocks.mu.Lock()
	defer replySocks.mu.Unlock()
	return r.byCID[cid]
}
