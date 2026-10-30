package mqtt

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func TestEncodeConnectWillCarriesTheWill(t *testing.T) {
	b := encodeConnectWill("reostream-events", "", "", 0, "reostream/status", []byte("offline"))
	// clean session, will flag, will retain; QoS 0.
	if !bytes.Contains(b, []byte{0x04, 0x26, 0x00, 0x00}) {
		t.Errorf("CONNECT flags wrong: % x", b)
	}
	want := append(encodeString("reostream/status"), 0x00, 0x07)
	want = append(want, "offline"...)
	if !bytes.Contains(b, want) {
		t.Errorf("CONNECT payload lacks the will: % x", b)
	}
}

// TestPresenceSaysOnlineThenOffline drives a client with a presence topic
// against an in-process broker and checks the order of what arrives: the
// will in CONNECT, online as soon as the connection is up, the message, and
// on Close offline followed by DISCONNECT, which makes the broker drop the
// will instead of publishing it later.
func TestPresenceSaysOnlineThenOffline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type packet struct {
		typ  byte
		body []byte
	}
	got := make(chan packet, 8)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			typ, body, err := readPacket(br)
			if err != nil {
				close(got)
				return
			}
			got <- packet{typ, body}
			if typ>>4 == 1 {
				conn.Write([]byte{0x20, 0x02, 0x00, 0x00})
			}
		}
	}()

	c := New(ln.Addr().String(), "t", "", "")
	c.SetPresence("reostream/status", "online", "offline")
	if err := c.Publish("reostream/cam/motion", []byte("ON"), 0, true); err != nil {
		t.Fatal(err)
	}
	c.Close()

	var seq []string
	timeout := time.After(2 * time.Second)
	for {
		select {
		case p, ok := <-got:
			if !ok {
				goto done
			}
			switch p.typ >> 4 {
			case 1:
				seq = append(seq, "CONNECT")
			case 3:
				tl := int(p.body[0])<<8 | int(p.body[1])
				seq = append(seq, string(p.body[2:2+tl])+"="+string(p.body[2+tl:]))
			case 14:
				seq = append(seq, "DISCONNECT")
			}
		case <-timeout:
			t.Fatalf("timed out; got %v", seq)
		}
	}
done:
	want := []string{"CONNECT", "reostream/status=online", "reostream/cam/motion=ON", "reostream/status=offline", "DISCONNECT"}
	if len(seq) != len(want) {
		t.Fatalf("got %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("got %v, want %v", seq, want)
		}
	}
}
