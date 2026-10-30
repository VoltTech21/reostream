// Package fakenvr fakes a Reolink NVR's Baichuan port for tests: it logs a
// client in, then serves every preview the client asks for, as many as it
// asks for and on as many channels, interleaved on the one connection the
// way a recorder interleaves them.
//
// Unlike internal/fakecam, which replays bytes and knows nothing of the
// protocol, a recorder has to read what the client asks for to know which
// channels to send, so this package decodes the client's requests with
// internal/baichuan. The media it sends is a real capture's, its channel
// and stream bytes rewritten per preview, so a client decodes real frames.
//
// The capture must be one internal/baichuan/testdata holds, made under an
// empty password: the recorder logs in with the capture's own nonce, so the
// client's AES key matches the key the capture's media was sealed with.
package fakenvr

import (
	"bytes"
	"encoding/xml"
	"net"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// deviceInfo is the login reply: an NVR, by the fields a client reads to
// tell a recorder from a camera.
const deviceInfo = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<DeviceInfo version="1.1">
<type>nvr</type>
<channelNum>12</channelNum>
<typeInfo>NVR</typeInfo>
</DeviceInfo>
</body>
`

// support is the recorder's Support document, cut down.
const support = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<Support version="1.1">
<channelNum>12</channelNum>
</Support>
</body>
`

// Preview names one stream a client asked for.
type Preview struct {
	Channel int
	Stream  string
}

// Recorder is a fake NVR on a loopback address.
type Recorder struct {
	ln    net.Listener
	nonce []byte // the capture's nonce reply, verbatim
	reply baichuan.Header
	key   []byte
	media [][]byte

	// Gap is the pause between two media messages of one preview.
	Gap time.Duration

	mu       sync.Mutex
	accepts  int
	open     int
	starts   []Preview
	stops    []Preview
	silent   map[int]bool
	conns    map[net.Conn]bool
	maxConns int
}

// New starts a fake recorder serving the media in capture, a server-side
// capture from internal/baichuan/testdata that begins with a login.
func New(t testing.TB, capture []byte) *Recorder {
	t.Helper()
	r := &Recorder{Gap: time.Millisecond, silent: map[int]bool{}, conns: map[net.Conn]bool{}}
	off, n := 0, 0
	for off < len(capture) {
		h, hl, err := baichuan.DecodeHeader(capture[off:])
		if err != nil || off+hl+int(h.MsgLen) > len(capture) {
			break
		}
		msg := capture[off : off+hl+int(h.MsgLen)]
		switch {
		case n == 0:
			r.nonce = msg
		case n == 1:
			r.reply = h
			// The document goes out whole, with no binary section.
			r.reply.PayloadOff = 0
		case h.MsgID == baichuan.MsgIDVideo:
			r.media = append(r.media, msg)
		}
		n++
		off += hl + int(h.MsgLen)
	}
	if r.nonce == nil || len(r.media) == 0 {
		t.Fatal("fakenvr: capture has no login or no media")
	}
	m, err := baichuan.NewReader(bytes.NewReader(r.nonce)).Next()
	if err != nil {
		t.Fatalf("fakenvr: nonce reply: %v", err)
	}
	found := regexp.MustCompile(`<nonce>([^<]+)</nonce>`).FindSubmatch(m.XML)
	if found == nil {
		t.Fatal("fakenvr: no nonce in the capture's first message")
	}
	r.key = baichuan.AESKey(string(found[1]), "")
	// A capture ends partway through a packet. Looped whole, the client
	// would see that packet cut short once per loop, so the loop ends
	// before the capture's last packet instead.
	for i := len(r.media) - 1; i > 0; i-- {
		in := baichuan.NewReader(bytes.NewReader(r.media[i]))
		in.SetAESKey(r.key)
		if m, err := in.Next(); err == nil && m.StartsPacket && len(m.Payload) > 0 {
			r.media = r.media[:i]
			break
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakenvr: listen: %v", err)
	}
	r.ln = ln
	go r.accept()
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		for c := range r.conns {
			c.Close()
		}
		r.mu.Unlock()
	})
	return r
}

// Addr is the address to dial, host:port.
func (r *Recorder) Addr() string { return r.ln.Addr().String() }

// Accepts is how many connections the recorder has accepted.
func (r *Recorder) Accepts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepts
}

// Open is how many connections are open now.
func (r *Recorder) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open
}

// MaxOpen is the most connections that were ever open at once.
func (r *Recorder) MaxOpen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxConns
}

// Starts lists every preview request, in order.
func (r *Recorder) Starts() []Preview {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Preview(nil), r.starts...)
}

// Stops lists every stream-stop request, in order.
func (r *Recorder) Stops() []Preview {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Preview(nil), r.stops...)
}

// Silence stops sending channel's media, on every preview of it, while
// leaving the connection and every other channel running: a camera that
// has gone dark behind the recorder.
func (r *Recorder) Silence(channel int, on bool) {
	r.mu.Lock()
	r.silent[channel] = on
	r.mu.Unlock()
}

// DropAll closes every open connection, as a recorder rebooting would.
func (r *Recorder) DropAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.conns {
		c.Close()
	}
}

func (r *Recorder) isSilent(channel int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.silent[channel]
}

func (r *Recorder) accept() {
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		r.accepts++
		r.open++
		if r.open > r.maxConns {
			r.maxConns = r.open
		}
		r.conns[c] = true
		r.mu.Unlock()
		go r.serve(c)
	}
}

type previewReq struct {
	Preview struct {
		ChannelID  int    `xml:"channelId"`
		Handle     int    `xml:"handle"`
		StreamType string `xml:"streamType"`
	} `xml:"Preview"`
}

func (r *Recorder) serve(c net.Conn) {
	defer func() {
		c.Close()
		r.mu.Lock()
		r.open--
		delete(r.conns, c)
		r.mu.Unlock()
	}()
	in := baichuan.NewReader(c)
	// The negotiation probe, then the login.
	if _, err := in.Next(); err != nil {
		return
	}
	if _, err := c.Write(r.nonce); err != nil {
		return
	}
	if _, err := in.Next(); err != nil {
		return
	}
	if err := baichuan.NewWriter(c).Write(r.reply, []byte(deviceInfo)); err != nil {
		return
	}
	in.SetAESKey(r.key)
	out := baichuan.NewWriter(c)
	out.SetAESKey(r.key)

	done := make(chan struct{})
	defer close(done)
	running := map[[2]int]chan struct{}{}
	for {
		m, err := in.Next()
		if err != nil {
			return
		}
		if m.Header.MsgID == baichuan.ConfigMessages["support"] {
			// Answered as the recorder answers it, minus the per-channel
			// items no test reads.
			h := baichuan.Header{
				MsgID:     m.Header.MsgID,
				Class:     baichuan.ClassModern24,
				EncOffset: m.Header.EncOffset,
				EncByte:   200,
			}
			if out.Write(h, []byte(support)) != nil {
				return
			}
			continue
		}
		if m.Header.MsgID != baichuan.MsgIDVideo {
			continue
		}
		var req previewReq
		if xml.Unmarshal(m.XML, &req) != nil {
			continue
		}
		key := [2]int{req.Preview.ChannelID, req.Preview.Handle}
		p := Preview{Channel: req.Preview.ChannelID, Stream: req.Preview.StreamType}
		if req.Preview.StreamType == "" {
			r.mu.Lock()
			r.stops = append(r.stops, p)
			r.mu.Unlock()
			if stop := running[key]; stop != nil {
				close(stop)
				delete(running, key)
			}
			continue
		}
		r.mu.Lock()
		r.starts = append(r.starts, p)
		r.mu.Unlock()
		stop := make(chan struct{})
		running[key] = stop
		// The reply carries the request's own offset, which is how a
		// client tells which preview a message belongs to.
		go r.send(c, m.Header.EncOffset, p.Channel, stop, done)
	}
}

// send loops the capture's media to one preview, tagged with the request's
// encryption offset.
func (r *Recorder) send(c net.Conn, offset uint32, channel int, stop, done <-chan struct{}) {
	for i := 0; ; i = (i + 1) % len(r.media) {
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-time.After(r.Gap):
		}
		if r.isSilent(channel) {
			continue
		}
		msg := append([]byte(nil), r.media[i]...)
		msg[12], msg[13], msg[14], msg[15] = byte(offset), byte(offset>>8), byte(offset>>16), byte(offset>>24)
		if _, err := c.Write(msg); err != nil {
			return
		}
	}
}
