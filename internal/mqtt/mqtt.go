// Package mqtt is a minimal MQTT 3.1.1 client: enough to feed motion and
// status into mosquitto / Home Assistant / Frigate, and to hear another
// tool's motion (Subscribe), without pulling in a full client library. A
// Client publishes QoS 0 or QoS 1, reconnects on the next Publish after any
// error, and uses keep-alive 0 so an idle connection is never dropped by the
// broker; Subscribe keeps its own connection alive with pings.
package mqtt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Client publishes to a single broker. It is safe for concurrent use; each
// Publish is serialized. A dropped connection is re-established on the next
// Publish, so a broker restart heals itself.
type Client struct {
	addr     string
	clientID string
	user     string
	pass     string
	dialer   net.Dialer

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
	pid  uint16

	// presence, when set, is the topic every connection announces itself
	// on; see SetPresence.
	presence        string
	online, offline []byte

	// shut is set by Shutdown: Publish refuses from then on.
	shut bool
}

// errShutdown is what Publish answers after Shutdown.
var errShutdown = errors.New("mqtt: client shut down")

// New returns a client for broker addr ("host:port"). clientID must be unique
// on the broker; user and pass may be empty for an anonymous broker.
func New(addr, clientID, user, pass string) *Client {
	return &Client{
		addr:     addr,
		clientID: clientID,
		user:     user,
		pass:     pass,
		dialer:   net.Dialer{Timeout: 5 * time.Second},
	}
}

// SetPresence makes every connection this client opens say whether it is
// up: the broker is asked to publish offline to topic, retained, if the
// connection is lost without a goodbye (MQTT's last will), and online is
// published there, retained, as each connection comes up. That is what
// Home Assistant reads as availability. Call it before the first Publish.
func (c *Client) SetPresence(topic, online, offline string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.presence, c.online, c.offline = topic, []byte(online), []byte(offline)
}

// Publish sends payload to topic. qos is 0 or 1; a QoS 1 publish waits for the
// broker's PUBACK. retain asks the broker to keep the message as the topic's
// last known value.
func (c *Client) Publish(topic string, payload []byte, qos byte, retain bool) error {
	if qos > 1 {
		return errors.New("mqtt: only QoS 0 and 1 are supported")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shut {
		return errShutdown
	}

	// Two attempts: a connection that the broker has silently closed is only
	// discovered when a write or PUBACK read fails, so the first attempt may
	// die on a stale socket. Dropping it and retrying publishes on a fresh
	// connection, which heals a broker restart without losing the event.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = c.ensure(); err != nil {
			continue
		}
		if err = c.send(topic, payload, qos, retain); err == nil {
			return nil
		}
		c.drop()
	}
	return err
}

func (c *Client) send(topic string, payload []byte, qos byte, retain bool) error {
	var id uint16
	if qos == 1 {
		c.pid++
		if c.pid == 0 {
			c.pid = 1
		}
		id = c.pid
	}
	if err := c.write(encodePublish(topic, payload, qos, retain, id)); err != nil {
		return err
	}
	if qos == 1 {
		return c.readPuback(id)
	}
	return nil
}

// Shutdown is Close for good: a Publish afterwards fails instead of opening
// a new connection. A client with a presence topic that is being replaced
// needs this, because a late Publish from some goroutine that has not
// noticed yet would reconnect it and say online again, after the
// replacement's own online and under the same client id.
func (c *Client) Shutdown() {
	c.Close()
	c.mu.Lock()
	c.shut = true
	c.mu.Unlock()
}

// Close drops the connection. The client may be reused afterwards.
//
// With a presence topic set it says offline first and then disconnects
// properly, which tells the broker to discard the last will. Without the
// DISCONNECT the broker would publish the will as well, at a moment of its
// choosing -- possibly after a replacement client has already said online.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.presence != "" {
		_ = c.send(c.presence, c.offline, 0, true)
		_ = c.write([]byte{0xe0, 0x00})
	}
	c.drop()
	return nil
}

func (c *Client) ensure() error {
	if c.conn != nil {
		return nil
	}
	conn, err := c.dialer.Dial("tcp", c.addr)
	if err != nil {
		return err
	}
	c.conn = conn
	c.br = bufio.NewReader(conn)
	connect := encodeConnect(c.clientID, c.user, c.pass)
	if c.presence != "" {
		connect = encodeConnectWill(c.clientID, c.user, c.pass, 0, c.presence, c.offline)
	}
	if err := c.write(connect); err != nil {
		c.drop()
		return err
	}
	if err := c.readConnack(); err != nil {
		c.drop()
		return err
	}
	if c.presence != "" {
		if err := c.send(c.presence, c.online, 0, true); err != nil {
			c.drop()
			return err
		}
	}
	return nil
}

func (c *Client) write(b []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(b)
	return err
}

func (c *Client) drop() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.br = nil
	}
}

func (c *Client) readConnack() error {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, body, err := readPacket(c.br)
	if err != nil {
		return err
	}
	if typ>>4 != 2 || len(body) < 2 {
		return fmt.Errorf("mqtt: expected CONNACK, got type %d", typ>>4)
	}
	if body[1] != 0 {
		return fmt.Errorf("mqtt: connection refused, code %d", body[1])
	}
	return nil
}

func (c *Client) readPuback(id uint16) error {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, body, err := readPacket(c.br)
		if err != nil {
			return err
		}
		if typ>>4 != 4 { // not PUBACK; ignore anything else the broker sends
			continue
		}
		if len(body) < 2 || uint16(body[0])<<8|uint16(body[1]) != id {
			return errors.New("mqtt: PUBACK packet id mismatch")
		}
		return nil
	}
}

// --- wire encoding (MQTT 3.1.1) ---

func encodeConnect(clientID, user, pass string) []byte {
	return encodeConnectKeepAlive(clientID, user, pass, 0)
}

// encodeConnectKeepAlive is CONNECT with a keep-alive, which a client that
// pings (Subscribe) can ask for; 0 disables it.
func encodeConnectKeepAlive(clientID, user, pass string, keepAlive time.Duration) []byte {
	return encodeConnectWill(clientID, user, pass, keepAlive, "", nil)
}

// encodeConnectWill is CONNECT with a last will, published retained at
// QoS 0 to willTopic when the connection is lost without a DISCONNECT. An
// empty willTopic is no will.
func encodeConnectWill(clientID, user, pass string, keepAlive time.Duration, willTopic string, will []byte) []byte {
	var vh []byte
	vh = append(vh, encodeString("MQTT")...)
	vh = append(vh, 0x04) // protocol level 4 (3.1.1)
	flags := byte(0x02)   // clean session
	if user != "" {
		flags |= 0x80
	}
	if pass != "" {
		flags |= 0x40
	}
	if willTopic != "" {
		flags |= 0x04 | 0x20 // will, will retain; will QoS 0
	}
	vh = append(vh, flags)
	ka := uint16(keepAlive / time.Second)
	vh = append(vh, byte(ka>>8), byte(ka))

	payload := encodeString(clientID)
	if willTopic != "" {
		payload = append(payload, encodeString(willTopic)...)
		payload = append(payload, byte(len(will)>>8), byte(len(will)))
		payload = append(payload, will...)
	}
	if user != "" {
		payload = append(payload, encodeString(user)...)
	}
	if pass != "" {
		payload = append(payload, encodeString(pass)...)
	}
	return frame(0x10, append(vh, payload...))
}

func encodePublish(topic string, payload []byte, qos byte, retain bool, id uint16) []byte {
	hdr := byte(0x30) | qos<<1
	if retain {
		hdr |= 0x01
	}
	body := encodeString(topic)
	if qos > 0 {
		body = append(body, byte(id>>8), byte(id))
	}
	body = append(body, payload...)
	return frame(hdr, body)
}

func frame(hdr byte, body []byte) []byte {
	out := []byte{hdr}
	out = append(out, encodeRemaining(len(body))...)
	return append(out, body...)
}

func encodeString(s string) []byte {
	b := make([]byte, 2+len(s))
	b[0] = byte(len(s) >> 8)
	b[1] = byte(len(s))
	copy(b[2:], s)
	return b
}

func encodeRemaining(n int) []byte {
	var b []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		b = append(b, d)
		if n == 0 {
			return b
		}
	}
}

// readPacket reads one control packet, returning its first byte (type and
// flags) and its remaining-length body.
func readPacket(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, err := readRemaining(r)
	if err != nil {
		return 0, nil, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func readRemaining(r *bufio.Reader) (int, error) {
	value, mult := 0, 1
	for i := 0; i < 4; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value += int(b&0x7f) * mult
		if b&0x80 == 0 {
			return value, nil
		}
		mult *= 128
	}
	return 0, errors.New("mqtt: remaining length too long")
}
