package bcudp

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// useReplyPort sets the reply port for one test and puts the default back
// after it.
func useReplyPort(t *testing.T, port int) {
	t.Helper()
	SetReplyPort(port)
	t.Cleanup(func() {
		replyPortSet.mu.Lock()
		replyPortSet.port, replyPortSet.set = 0, false
		replyPortSet.mu.Unlock()
	})
}

// freePort is a UDP port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func openReplySocks() int {
	replySocks.mu.Lock()
	defer replySocks.mu.Unlock()
	return len(replySocks.m)
}

func TestReplyPortIsNamed(t *testing.T) {
	port := freePort(t)
	useReplyPort(t, port)
	f := newFakeCam(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, err := search(ctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.port()}, true)
	if err != nil || len(found) != 1 || found[0].UID != testUID || !found[0].Addr.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("%+v %v", found, err)
	}
	c := dialFake(t, f)
	if got := c.LocalAddr().(*net.UDPAddr).Port; got != port {
		t.Fatalf("session on port %d, want the reply port %d", got, port)
	}

	f.send([]byte("over the reply port"))
	got := make([]byte, 19)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "over the reply port" {
		t.Fatalf("%q %v", got, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.searchedPort != port || f.connectedPort != port {
		t.Fatalf("search named %d and connect %d, want %d", f.searchedPort, f.connectedPort, port)
	}
}

func TestReplyPortTwoSessions(t *testing.T) {
	useReplyPort(t, freePort(t))
	// Both cameras are on 127.0.0.1 with the same did: only the cid tells
	// their packets apart.
	f1, f2 := newFakeCam(t), newFakeCam(t)
	c1, c2 := dialFake(t, f1), dialFake(t, f2)
	if c1.LocalAddr().String() != c2.LocalAddr().String() {
		t.Fatalf("sessions on %s and %s, want one shared socket", c1.LocalAddr(), c2.LocalAddr())
	}

	down1, down2 := randomBytes(100_000), randomBytes(100_000)
	f1.send(down1)
	f2.send(down2)
	up1, up2 := []byte("to camera one"), []byte("to camera two")
	c1.Write(up1)
	c2.Write(up2)
	for _, x := range []struct {
		c    *Conn
		want []byte
	}{{c1, down1}, {c2, down2}} {
		got := make([]byte, len(x.want))
		_ = x.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(x.c, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, x.want) {
			t.Fatal("a session read another camera's stream")
		}
	}
	waitGot(t, f1, up1)
	waitGot(t, f2, up2)

	// Closing one leaves the other running, and the last close frees the
	// port.
	c1.Close()
	f2.send([]byte("still here"))
	got := make([]byte, 10)
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c2, got); err != nil || string(got) != "still here" {
		t.Fatalf("after the other session closed: %q %v", got, err)
	}
	c2.Close()
	if n := openReplySocks(); n != 0 {
		t.Fatalf("%d reply sockets open after every session closed", n)
	}
}

func waitGot(t *testing.T, f *fakeCam, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		ok := bytes.Equal(f.got, want)
		got := string(f.got)
		f.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera got %q, want %q", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNoReplyPortNamesOwnPort(t *testing.T) {
	t.Setenv(ReplyPortEnv, "")
	f := newFakeCam(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := search(ctx, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.port()}, true); err != nil {
		t.Fatal(err)
	}
	c := dialFake(t, f)
	own := c.LocalAddr().(*net.UDPAddr).Port
	f.mu.Lock()
	connected, searched := f.connectedPort, f.searchedPort
	f.mu.Unlock()
	if connected != own {
		t.Fatalf("connect named %d, want the session's own port %d", connected, own)
	}
	if searched == 0 || searched == own {
		t.Fatalf("search named %d", searched)
	}
	if c.reply != nil || openReplySocks() != 0 {
		t.Fatal("a reply socket opened with no reply port set")
	}
}

func TestReplyPortFromEnv(t *testing.T) {
	port := freePort(t)
	t.Setenv(ReplyPortEnv, strconv.Itoa(port))
	f := newFakeCam(t)
	c := dialFake(t, f)
	if got := c.LocalAddr().(*net.UDPAddr).Port; got != port {
		t.Fatalf("session on port %d, want %d from %s", got, port, ReplyPortEnv)
	}

	t.Setenv(ReplyPortEnv, "forty-one thousand")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dial(ctx, "127.0.0.1", testUID, []int{f.port()}); err == nil || !strings.Contains(err.Error(), ReplyPortEnv) {
		t.Fatalf("a bad %s gave %v", ReplyPortEnv, err)
	}
}

func TestReplyPortInUse(t *testing.T) {
	busy, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	useReplyPort(t, busy.LocalAddr().(*net.UDPAddr).Port)
	f := newFakeCam(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := dial(ctx, "127.0.0.1", testUID, []int{f.port()}); err == nil {
		t.Fatal("dialed with the reply port taken")
	}
	if Ours(net.IPv4(127, 0, 0, 1), 0) {
		t.Fatal("a failed dial left the camera noted as ours")
	}
}
