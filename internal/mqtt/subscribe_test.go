package mqtt

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// fakeBroker accepts one connection at a time, answers CONNECT, checks the
// SUBSCRIBE and answers it, then sends each of pubs. It returns the topics
// each connection subscribed to, in order.
func fakeBroker(t *testing.T, ln net.Listener, pubs [][]byte, subscribed chan<- []string) {
	t.Helper()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			br := bufio.NewReader(conn)
			if typ, _, err := readPacket(br); err != nil || typ>>4 != 1 {
				t.Errorf("expected CONNECT, got type %d err %v", typ>>4, err)
				return
			}
			conn.Write([]byte{0x20, 0x02, 0x00, 0x00})
			typ, body, err := readPacket(br)
			if err != nil || typ != 0x82 {
				t.Errorf("expected SUBSCRIBE (0x82), got %#x err %v", typ, err)
				return
			}
			id := body[:2]
			var topics []string
			for rest := body[2:]; len(rest) >= 3; {
				n := int(rest[0])<<8 | int(rest[1])
				topics = append(topics, string(rest[2:2+n]))
				rest = rest[2+n+1:]
			}
			subscribed <- topics
			suback := []byte{0x90, byte(2 + len(topics)), id[0], id[1]}
			for range topics {
				suback = append(suback, 0x00)
			}
			conn.Write(suback)
			for _, p := range pubs {
				conn.Write(p)
			}
			// Hold the connection until the client goes away, answering pings
			// and swallowing PUBACKs.
			for {
				typ, _, err := readPacket(br)
				if err != nil {
					return
				}
				if typ>>4 == 12 {
					conn.Write([]byte{0xd0, 0x00})
				}
			}
		}(conn)
	}
}

func TestSubscribeDeliversMessages(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	subscribed := make(chan []string, 4)
	go fakeBroker(t, ln, [][]byte{
		encodePublish("reostream/argus/motion", []byte("ON"), 0, false, 0),
		encodePublish("reostream/argus/motion", []byte("OFF"), 1, false, 7),
	}, subscribed)

	type msg struct{ topic, payload string }
	got := make(chan msg, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Subscribe(ctx, SubscribeOptions{Addr: ln.Addr().String(), ClientID: "t"}, []string{"reostream/argus/motion"},
		func(topic string, payload []byte) { got <- msg{topic, string(payload)} })

	select {
	case topics := <-subscribed:
		if len(topics) != 1 || topics[0] != "reostream/argus/motion" {
			t.Fatalf("subscribed to %v", topics)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never subscribed")
	}
	for _, want := range []string{"ON", "OFF"} {
		select {
		case m := <-got:
			if m.topic != "reostream/argus/motion" || m.payload != want {
				t.Fatalf("got %+v, want payload %s", m, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("message %s never delivered", want)
		}
	}
}

// A broker restart must not end the subscription: the client reconnects and
// subscribes again.
func TestSubscribeResubscribesAfterTheBrokerDrops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	subscribed := make(chan []string, 4)
	// A DISCONNECT-less close right after SUBACK: the broker goes away.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(conn)
		readPacket(br)
		conn.Write([]byte{0x20, 0x02, 0x00, 0x00})
		_, body, _ := readPacket(br)
		subscribed <- []string{"first"}
		conn.Write([]byte{0x90, 0x03, body[0], body[1], 0x00})
		conn.Close()
		fakeBroker(t, ln, nil, subscribed)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Subscribe(ctx, SubscribeOptions{Addr: ln.Addr().String(), ClientID: "t", Retry: 50 * time.Millisecond},
		[]string{"a/b"}, func(string, []byte) {})
	for i := 0; i < 2; i++ {
		select {
		case <-subscribed:
		case <-time.After(3 * time.Second):
			t.Fatalf("subscription %d never arrived", i+1)
		}
	}
}

func TestSubscribeStopsWithItsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	subscribed := make(chan []string, 4)
	go fakeBroker(t, ln, nil, subscribed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Subscribe(ctx, SubscribeOptions{Addr: ln.Addr().String(), ClientID: "t"}, []string{"a/b"}, func(string, []byte) {})
		close(done)
	}()
	<-subscribed
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe did not return after its context ended")
	}
}

// A command topic must not act on a retained message the broker replays on
// subscribing: only what is published while listening counts.
func TestSubscribeSkipsRetainedWhenAsked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	subscribed := make(chan []string, 4)
	go fakeBroker(t, ln, [][]byte{
		encodePublish("reostream/cam/siren/set", []byte("STALE"), 0, true, 0),
		encodePublish("reostream/cam/siren/set", []byte("PRESS"), 0, false, 0),
	}, subscribed)
	got := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Subscribe(ctx, SubscribeOptions{Addr: ln.Addr().String(), ClientID: "t", SkipRetained: true}, []string{"reostream/cam/siren/set"},
		func(_ string, payload []byte) { got <- string(payload) })
	select {
	case p := <-got:
		if p != "PRESS" {
			t.Fatalf("got %q first, want the retained message skipped", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing delivered")
	}
}
