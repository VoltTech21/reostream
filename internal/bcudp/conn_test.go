package bcudp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

const testUID = "95270000TESTUID1"

// fakeCam is a battery camera on loopback: a control socket that answers
// C2D_C and C2D_S, and a session socket with its own resend and ack logic.
type fakeCam struct {
	ctl, sess *net.UDPConn
	did       uint32

	// dropOut and dropIn decide which first transmissions are lost.
	dropOut func(id uint32) bool
	dropIn  func(id uint32) bool

	mu       sync.Mutex
	client   *net.UDPAddr
	cid      uint32
	out      [][]byte
	outAcked map[uint32]bool
	outSent  map[uint32]time.Time
	inSeen   map[uint32]bool
	in       map[uint32][]byte
	inNext   uint32
	got      []byte
	discs    int
	hbs      int
	// searchedPort and connectedPort are the ports the last C2D_S and
	// C2D_C named for the reply.
	searchedPort  int
	connectedPort int
	done          chan struct{}
}

func newFakeCam(t *testing.T) *fakeCam {
	t.Helper()
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	f := &fakeCam{
		ctl: listen(), sess: listen(), did: 0x5151,
		dropOut:  func(uint32) bool { return false },
		dropIn:   func(uint32) bool { return false },
		outAcked: map[uint32]bool{}, outSent: map[uint32]time.Time{},
		inSeen: map[uint32]bool{}, in: map[uint32][]byte{},
		done: make(chan struct{}),
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); f.ctlLoop() }()
	go func() { defer wg.Done(); f.sessLoop() }()
	go func() { defer wg.Done(); f.timerLoop() }()
	t.Cleanup(func() {
		close(f.done)
		f.ctl.Close()
		f.sess.Close()
		wg.Wait()
	})
	return f
}

func (f *fakeCam) port() int { return f.ctl.LocalAddr().(*net.UDPAddr).Port }

func (f *fakeCam) ctlLoop() {
	buf := make([]byte, 65536)
	for {
		n, from, err := f.ctl.ReadFromUDP(buf)
		if err != nil {
			return
		}
		tid, body, err := decodeDiscovery(buf[:n])
		if err != nil {
			continue
		}
		m, err := parseP2P(body)
		if err != nil {
			continue
		}
		switch {
		case m.Search != nil:
			f.mu.Lock()
			f.searchedPort = m.Search.Port
			f.mu.Unlock()
			reply := []byte("<P2P><D2C_S_R><bat><low>0</low><pct>57</pct><c>0</c><p>0</p></bat><uid>" + testUID + "</uid></D2C_S_R></P2P>")
			f.ctl.WriteToUDP(encodeDiscovery(tid, reply), &net.UDPAddr{IP: from.IP, Port: m.Search.Port})
		case m.Connect != nil && m.Connect.UID == testUID:
			f.mu.Lock()
			f.client = &net.UDPAddr{IP: from.IP, Port: m.Connect.Port}
			f.cid = m.Connect.CID
			f.connectedPort = m.Connect.Port
			f.mu.Unlock()
			reply := render("D2C_C_R", tag("rsp", 0), tag("cid", m.Connect.CID), tag("did", f.did))
			f.sess.WriteToUDP(encodeDiscovery(tid, reply), f.client)
		}
	}
}

func (f *fakeCam) sessLoop() {
	buf := make([]byte, 65536)
	for {
		n, _, err := f.sess.ReadFromUDP(buf)
		if err != nil {
			return
		}
		b := buf[:n]
		f.mu.Lock()
		switch magicOf(b) {
		case magicData:
			p, err := decodeData(b)
			if err != nil || p.conn != f.did {
				break
			}
			first := !f.inSeen[p.id]
			f.inSeen[p.id] = true
			if first && f.dropIn(p.id) {
				break
			}
			if p.id >= f.inNext {
				f.in[p.id] = append([]byte(nil), p.payload...)
			}
			for {
				next, ok := f.in[f.inNext]
				if !ok {
					break
				}
				delete(f.in, f.inNext)
				f.got = append(f.got, next...)
				f.inNext++
			}
		case magicAck:
			a, err := decodeAck(b)
			if err != nil || a.conn != f.did {
				break
			}
			base := a.last + 1
			for id := uint32(0); int32(id-base) < 0 && int(id) < len(f.out); id++ {
				f.outAcked[id] = true
			}
			for i, h := range a.have {
				if h == 1 {
					f.outAcked[base+uint32(i)] = true
				}
			}
		case magicDiscovery:
			_, body, err := decodeDiscovery(b)
			if err != nil {
				break
			}
			m, _ := parseP2P(body)
			if m.CDisc != nil {
				f.discs++
			}
			if m.CHB != nil {
				f.hbs++
			}
		}
		f.mu.Unlock()
	}
}

func (f *fakeCam) timerLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
		}
		f.mu.Lock()
		if f.client == nil {
			f.mu.Unlock()
			continue
		}
		var pkts [][]byte
		a := ackPacket{conn: f.cid, last: f.inNext - 1}
		pkts = append(pkts, encodeAck(a))
		now := time.Now()
		for id, payload := range f.out {
			id := uint32(id)
			if f.outAcked[id] {
				continue
			}
			sent, ok := f.outSent[id]
			if ok && now.Sub(sent) < 100*time.Millisecond {
				continue
			}
			f.outSent[id] = now
			if !ok && f.dropOut(id) {
				continue
			}
			pkts = append(pkts, encodeData(f.cid, id, payload))
		}
		client := f.client
		f.mu.Unlock()
		for _, p := range pkts {
			f.sess.WriteToUDP(p, client)
		}
	}
}

func (f *fakeCam) send(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for off := 0; off < len(b); off += maxPayload {
		f.out = append(f.out, append([]byte(nil), b[off:min(off+maxPayload, len(b))]...))
	}
}

func (f *fakeCam) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess.WriteToUDP(encodeDiscovery(7, sessionXML("D2C_DISC", f.cid, f.did)), f.client)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.Uint32())
	}
	return b
}

func dialFake(t *testing.T, f *fakeCam) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dial(ctx, "127.0.0.1", testUID, []int{f.port()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStreamSurvivesLoss(t *testing.T) {
	f := newFakeCam(t)
	f.dropOut = func(id uint32) bool { return id%5 == 2 }
	f.dropIn = func(id uint32) bool { return id%4 == 1 }
	c := dialFake(t, f)

	down := randomBytes(300_000)
	f.send(down)
	up := randomBytes(60_000)
	go func() {
		for off := 0; off < len(up); off += 7000 {
			c.Write(up[off:min(off+7000, len(up))])
		}
	}()

	got := make([]byte, len(down))
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, down) {
		t.Fatal("camera's stream arrived corrupted")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		done := bytes.Equal(f.got, up)
		n := len(f.got)
		f.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera has %d of %d bytes", n, len(up))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discs == 0 {
		t.Fatal("Close sent no C2D_DISC")
	}
	if f.hbs == 0 {
		t.Fatal("no heartbeat in the session")
	}
}

func TestWrongUIDTimesOut(t *testing.T) {
	f := newFakeCam(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, err := dial(ctx, "127.0.0.1", "95270000WRONGUID", []int{f.port()})
	if err == nil {
		c.Close()
		t.Fatal("a wrong UID connected")
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("gave up after %s, before the deadline", d)
	}
}

func TestDisconnectReadsAsEOF(t *testing.T) {
	f := newFakeCam(t)
	c := dialFake(t, f)
	f.send([]byte("last words"))
	got := make([]byte, 10)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "last words" {
		t.Fatalf("%q %v", got, err)
	}
	f.disconnect()
	if _, err := c.Read(got); err != io.EOF {
		t.Fatalf("after D2C_DISC Read = %v, want EOF", err)
	}
}

func TestReadDeadline(t *testing.T) {
	f := newFakeCam(t)
	c := dialFake(t, f)
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := c.Read(make([]byte, 10))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read = %v", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatal("deadline error is not a net timeout")
	}

	// A deadline moved while a Read waits takes effect.
	_ = c.SetReadDeadline(time.Time{})
	errc := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 10))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now())
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a moved deadline did not wake Read")
	}
}

func TestSearch(t *testing.T) {
	f := newFakeCam(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, err := search(ctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.port()}, true)
	if err != nil || len(found) != 1 {
		t.Fatalf("%v %v", found, err)
	}
	if got := found[0]; got.UID != testUID || !got.Asleep || got.Battery != 57 || !got.Addr.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("%+v", got)
	}
}
