package mqtt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// SubscribeOptions is the broker and login for Subscribe.
type SubscribeOptions struct {
	Addr     string
	ClientID string
	User     string
	Pass     string
	// Retry is the pause before reconnecting after the connection ends,
	// 5 s when zero.
	Retry time.Duration
	// OnError, when set, hears why each connection ended.
	OnError func(error)
	// SkipRetained drops the retained messages a broker replays on
	// subscribing. A command topic needs it: a retained "PRESS" would
	// sound a siren every time the connection came back.
	SkipRetained bool
}

// subscribeKeepAlive is the keep-alive a subscriber asks for. Unlike a
// publisher it sits idle for hours, so it pings at half this to keep the
// broker and any NAT on the way from dropping it.
const subscribeKeepAlive = 60 * time.Second

// Subscribe listens on topics (QoS 0 and 1) and calls fn with each message
// until ctx ends, reconnecting and subscribing again whenever the connection
// drops. fn runs on the reading goroutine, so it should not block for long.
func Subscribe(ctx context.Context, o SubscribeOptions, topics []string, fn func(topic string, payload []byte)) {
	retry := o.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for {
		err := subscribeOnce(ctx, o, topics, fn)
		if ctx.Err() != nil {
			return
		}
		if o.OnError != nil && err != nil {
			o.OnError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func subscribeOnce(ctx context.Context, o SubscribeOptions, topics []string, fn func(string, []byte)) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", o.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	br := bufio.NewReader(conn)
	write := func(b []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Write(b)
		return err
	}
	if err := write(encodeConnectKeepAlive(o.ClientID, o.User, o.Pass, subscribeKeepAlive)); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, body, err := readPacket(br)
	if err != nil {
		return err
	}
	if typ>>4 != 2 || len(body) < 2 || body[1] != 0 {
		return fmt.Errorf("mqtt: connection refused (type %d)", typ>>4)
	}
	if err := write(encodeSubscribe(1, topics)); err != nil {
		return err
	}

	// Pings run beside the reader; a failed write closes the connection,
	// which ends the read below.
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(subscribeKeepAlive / 2)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if write([]byte{0xc0, 0x00}) != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		// The broker answers each ping, so a quiet connection still sees a
		// packet every subscribeKeepAlive/2.
		_ = conn.SetReadDeadline(time.Now().Add(subscribeKeepAlive + subscribeKeepAlive/2))
		typ, body, err := readPacket(br)
		if err != nil {
			return err
		}
		switch typ >> 4 {
		case 3: // PUBLISH
			if len(body) < 2 {
				return errors.New("mqtt: short PUBLISH")
			}
			n := int(body[0])<<8 | int(body[1])
			if len(body) < 2+n {
				return errors.New("mqtt: short PUBLISH topic")
			}
			topic, rest := string(body[2:2+n]), body[2+n:]
			if qos := (typ >> 1) & 0x03; qos > 0 {
				if len(rest) < 2 {
					return errors.New("mqtt: short PUBLISH id")
				}
				id := rest[:2]
				rest = rest[2:]
				if qos == 1 {
					if err := write([]byte{0x40, 0x02, id[0], id[1]}); err != nil {
						return err
					}
				}
			}
			if o.SkipRetained && typ&0x01 != 0 {
				continue
			}
			fn(topic, rest)
		case 9: // SUBACK
			for _, code := range body[min(2, len(body)):] {
				if code == 0x80 {
					return errors.New("mqtt: the broker refused the subscription")
				}
			}
		}
	}
}

func encodeSubscribe(id uint16, topics []string) []byte {
	body := []byte{byte(id >> 8), byte(id)}
	for _, t := range topics {
		body = append(body, encodeString(t)...)
		body = append(body, 0x01) // QoS 1
	}
	return frame(0x82, body)
}
