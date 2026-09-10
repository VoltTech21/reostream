package baichuan

import (
	"context"
	"errors"
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

	// IdleTimeout bounds how long a read may wait for the camera to send
	// anything before Conn gives up on it. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
}

// DefaultIdleTimeout is how long a post-login connection tolerates the
// camera sending nothing before readLoop gives up.
//
// docs/measurements.md and internal/stream/stream.go's own ping-cadence
// comment establish the baseline this is set against: on a live stream
// frames arrive roughly every 40ms, and the client pings the camera every
// 10 seconds purely to hold the session open (the ping is never acked, so it
// does not by itself put anything on the read side). A healthy camera
// therefore has no legitimate reason to be silent for more than a fraction
// of a second, ping cadence included. 15s is chosen as several multiples of
// that 10s ping interval, not tuned close to it, so ordinary jitter, a
// stalled TCP segment, or a camera stuttering under load cannot trip it, while
// still being short enough that a supervisor built on top of Conn notices a
// held or dead session in seconds rather than the minutes the 2026-09-08
// incident actually ran for.
const DefaultIdleTimeout = 15 * time.Second

// Conn is one Baichuan session carrying one stream.
//
// A camera permits one connection per stream (main, sub and extern are
// independent), so a Conn must be closed cleanly. Close sends the stream-stop
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

	// deviceInfo is the raw <DeviceInfo> document the camera sends as its
	// login reply on success (docs/protocol.md's handshake table, message
	// 4). login() populates it before Dial returns; every other read in
	// this package costs a further round trip, and this one does not,
	// because it arrives whether or not anything asks for it.
	deviceInfo []byte

	counter byte
	mu      sync.Mutex

	msgs      chan Message
	done      chan struct{}
	closeOnce sync.Once

	readMu  sync.Mutex
	readErr error

	idleTimeout time.Duration
}

// idleTimeoutConn resets the underlying connection's read deadline on every
// call to Read, not once per message.
//
// Reader.Next takes an io.Reader and issues several io.ReadFull calls per
// message (header, then body), each of which can itself take several
// syscall-level Reads if the peer trickles bytes in. A deadline set once
// before Next is called would therefore cap the time to receive one whole
// message, not the gap between bytes; the largest single message this
// protocol allows is a full 4K keyframe's worth of data (MaxMessageSize),
// and a deadline sized for "no data at all" would be far too short for that
// to arrive whole on a loaded network. Resetting on every Read instead makes
// the timeout genuinely an idle timeout: it only fires when nothing at all
// arrives for the whole window, regardless of how a message's bytes are
// split across reads.
type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
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
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}

	// idleConn wraps nc for the whole connection lifetime, but its timeout
	// field stays zero (disabled) through login so login keeps using the
	// dial context's own deadline below rather than having that overridden
	// by a Read call resetting the deadline to now+idle first. It is armed
	// only once login succeeds and the dial deadline is cleared, which is
	// safe to do without a lock: the field is written here, before
	// readLoop's goroutine is started, and never written again, so the
	// go statement's happens-before guarantee is all the synchronisation
	// this needs.
	idleConn := &idleTimeoutConn{Conn: nc}
	c := &Conn{
		nc:          nc,
		r:           NewReader(idleConn),
		w:           NewWriter(nc),
		opts:        opts,
		idleTimeout: idle,
		msgs:        make(chan Message, 256),
		done:        make(chan struct{}),
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	if err := c.login(); err != nil {
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	idleConn.timeout = idle
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

// ErrUnauthorised is what Dial returns when a camera's login reply carries
// no XML body.
//
// The wire gives no separate "wrong password" document: a rejected
// credential and an accepted one differ only in whether the login reply's
// body is empty. Read on its own, that symptom is indistinguishable from a
// camera holding a dead session (see DefaultIdleTimeout), which is exactly
// the confusion a first time user hits when a typed password is wrong. This
// sentinel is what lets a caller tell the two apart and say "authentication
// failed" instead of "something is wrong with the network".
var ErrUnauthorised = errors.New("baichuan: authentication rejected (empty login reply)")

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
		return fmt.Errorf("%w (status %d, msgid %d, class 0x%04x, %d body bytes)",
			ErrUnauthorised, reply.Header.Status(), reply.Header.MsgID, reply.Header.Class, len(reply.Payload))
	}
	c.deviceInfo = reply.XML

	key := AESKey(nonce, c.opts.Password)
	c.r.SetAESKey(key)
	c.w.SetAESKey(key)
	return nil
}

// Nonce reports the login nonce.
func (c *Conn) Nonce() string { return c.nonce }

// DeviceInfo returns the raw <DeviceInfo> document the camera sent as its
// login reply. Parse it with ParseDeviceInfo.
func (c *Conn) DeviceInfo() []byte { return c.deviceInfo }

// readLoop reads messages off the socket and hands them to Messages until
// the socket errors or Close closes done.
//
// The send to c.msgs is a select alongside done, not a plain send. Once
// Run's consumer above stops reading (Run returns and nothing services
// conn.Messages() again), the 256-slot buffer fills and a plain send parks
// here forever: Close closing the socket makes c.r.Next() fail on the *next*
// read, but this goroutine is not at that read, it is blocked trying to
// deliver the one before it, so it never notices. Selecting on done as well
// gives Close a way to wake a goroutine parked on the send, not just one
// parked on the read.
func (c *Conn) readLoop() {
	defer close(c.msgs)
	for {
		m, err := c.r.Next()
		if err != nil {
			// A deadline exceeded error here is idleTimeoutConn firing, not
			// a network failure, and the two need to read differently in an
			// operator's log: this one means the camera accepted a
			// connection and then never sent anything, the signature of a
			// held session (see DefaultIdleTimeout), while a plain network
			// error means the connection itself broke.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				err = fmt.Errorf("baichuan: idle timeout: no data from camera in %s: %w", c.idleTimeout, err)
			}
			c.readMu.Lock()
			c.readErr = err
			c.readMu.Unlock()
			return
		}
		select {
		case c.msgs <- m:
		case <-c.done:
			return
		}
	}
}

// Messages yields every message the camera sends after login.
func (c *Conn) Messages() <-chan Message { return c.msgs }

// Err reports the read error that ended the read loop and closed the
// channel from Messages, or nil if the loop is still running. After Close,
// this is the "use of closed network connection" error the read side sees,
// not a camera-side failure; callers that care about that distinction should
// check it before calling Close.
func (c *Conn) Err() error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.readErr
}

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

// TalkAbility asks the camera to describe its two-way audio support: the
// duplex modes, the stream modes and the audio encodings it will accept.
// The reply arrives on Messages as MsgIDTalkAbility.
func (c *Conn) TalkAbility() error {
	body, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:      MsgIDTalkAbility,
		Class:      ClassModern24,
		EncOffset:  EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
		PayloadOff: uint32(len(body)),
	}
	return c.w.Write(h, body)
}

// TalkConfig opens a two-way audio session in the format cfg describes.
// It must be sent before any Talk data, and cfg should come from the
// camera's own TalkAbility reply rather than from constants.
func (c *Conn) TalkConfig(cfg TalkFormat) error {
	ext, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	body, err := talkConfigXML(c.opts.Channel, cfg)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     MsgIDTalkConfig,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	return c.w.WriteParts(h, ext, body)
}

// Talk sends one framed audio packet, as built by TalkPacket. The camera
// plays it through its speaker, so this is the one call in this package with
// an effect outside the machine it runs on.
func (c *Conn) Talk(packet []byte) error {
	ext, err := talkDataXML(c.opts.Channel)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     MsgIDTalk,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	return c.w.WriteMedia(h, ext, packet)
}

// Snap asks the camera for a still image. The camera answers with one
// message naming the file and its size, then sends the JPEG bytes across as
// many further messages as it needs. Collect them with a SnapReader.
//
// stream is "main" or "sub" and selects which encoder the still comes from,
// so it is also the resolution control.
func (c *Conn) Snap(stream string) error {
	ext, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	body, err := snapXML(c.opts.Channel, stream)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     MsgIDSnap,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	return c.w.WriteParts(h, ext, body)
}

// RawChannelRequest sends a bare request carrying only a channel, for
// probing message ids whose shape is not yet known.
func (c *Conn) RawChannelRequest(msgID uint32) error {
	body, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:      msgID,
		Class:      ClassModern24,
		EncOffset:  EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
		PayloadOff: uint32(len(body)),
	}
	return c.w.Write(h, body)
}

// Abilities asks the camera what the logged in user may do, module by
// module. The reply arrives on Messages as MsgIDAbilityInfo; parse it with
// ParseAbilities.
//
// This is the Baichuan equivalent of the HTTP API's GetAbility, and it is
// worth preferring over a hardcoded assumption about a model. It is not
// infallible: the HTTP ability list reports "talk" on cameras that have no
// speaker at all.
func (c *Conn) Abilities() error {
	body, err := abilityXML(c.opts.Username)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:      MsgIDAbilityInfo,
		Class:      ClassModern24,
		EncOffset:  EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
		PayloadOff: uint32(len(body)),
	}
	return c.w.Write(h, body)
}

// GetConfig asks the camera for one configuration block and returns nothing;
// the reply arrives on Messages carrying the same message id, with the XML in
// Message.XML.
//
// Most read requests share one shape: an extension naming the channel and
// nothing else. That covers the LED state, the encoder settings, the ISP
// settings, the record schedule and the rest, so they need one implementation
// rather than one each. A message that wants more than a channel is not this,
// and needs its own method.
func (c *Conn) GetConfig(msgID uint32) error {
	body, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:      msgID,
		Class:      ClassModern24,
		EncOffset:  EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
		PayloadOff: uint32(len(body)),
	}
	return c.w.Write(h, body)
}

// SetConfig sends a configuration write.
//
// The body is the caller's XML. For every get/set pair the right body is the
// document the matching get returned with one field changed: the camera
// supplies its own schema, so nothing here needs to know a model's fields,
// and echoing the document back preserves the exact byte formatting that
// some messages insist on.
//
// The reply status says the camera accepted the message, not that it acted
// on it. Verify the effect, not the call.
func (c *Conn) SetConfig(msgID uint32, body []byte) error {
	// A write goes out in the shape TalkConfig uses: the channel in an
	// extension section, the document in a second section. Sent as one
	// section instead, a camera answers 421 and changes nothing.
	ext, err := channelXML(c.opts.Channel)
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     msgID,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	return c.w.WriteParts(h, ext, body)
}

// heartBeatTwoPart selects the message shape, for the experiment that
// settles it against a camera.
var heartBeatTwoPart = false

// SetHeartBeatTwoPart switches that choice.
func SetHeartBeatTwoPart(v bool) { heartBeatTwoPart = v }

// HeartBeat sends the real heartbeat, message 5, and returns nothing: the
// reply arrives on Messages and parses with ParseHeartBeat.
//
// Unlike Ping this is a round trip measurement rather than a liveness poke.
// The camera answers with its own clock and a count of overlapped requests,
// so it reports how far behind the client is rather than only that it is
// still there. The camera's side of the same mechanism is the log line
// "heartbeat timeout, session:%d chn:%d devname:%s disconnected".
//
// The message id is not in the dissector's table. It came from the camera
// firmware, where bc_module::heartbeat's failure path calls
// rpc_msg_name(5) to name the message it could not send.
func (c *Conn) HeartBeat() error {
	body, err := heartBeatXML()
	if err != nil {
		return err
	}
	h := Header{
		MsgID:     MsgIDHeartBeat,
		Class:     ClassModern24,
		EncOffset: EncOffsetFor(byte(c.opts.Channel), 0, c.nextCounter(), 0),
	}
	if heartBeatTwoPart {
		ext, err := channelXML(c.opts.Channel)
		if err != nil {
			return err
		}
		return c.w.WriteParts(h, ext, body)
	}
	h.PayloadOff = uint32(len(body))
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
		close(c.done)
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
