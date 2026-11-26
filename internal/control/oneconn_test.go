package control

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// resettingListener accepts and resets every connection, the way a D800 V2
// on firmware 3.1 answers a second connection while it has one (#44).
func resettingListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			c.SetReadDeadline(time.Now().Add(time.Second))
			c.Read(buf)
			c.(*net.TCPConn).SetLinger(0)
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestAResetDialIsARefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := baichuan.Dial(ctx, resettingListener(t), baichuan.Options{Username: "admin"})
	if err == nil {
		t.Fatal("dial to a resetting camera worked")
	}
	if !allRefused([]error{err, err}) {
		t.Fatalf("allRefused(%v) = false, want the reset recognised", err)
	}
	if allRefused([]error{err, errors.New("wrong username or password")}) {
		t.Fatal("a refusal beside a different failure counted as a one-connection camera")
	}
	if allRefused([]error{context.DeadlineExceeded}) || allRefused(nil) {
		t.Fatal("a timeout or no reads at all counted as refusals")
	}
}

type closeFunc func()

func (f closeFunc) Close() error { f(); return nil }

// With the login up every stream read is reset; once it is closed main
// answers. Main is offered alone and the other streams say why.
func TestRetryMainAloneOnAOneConnectionCamera(t *testing.T) {
	var loginOpen atomic.Bool
	loginOpen.Store(true)
	reset := errors.New("baichuan: read negotiation reply: read tcp 192.0.2.5:36294->192.0.2.9:9000: read: connection reset by peer")
	old := streamInfo
	defer func() { streamInfo = old }()
	streamInfo = func(ctx context.Context, addr string, opts baichuan.Options, kind string) (baichuan.StreamInfo, error) {
		if !loginOpen.Load() && kind == streamKinds["main"] {
			return baichuan.StreamInfo{Codec: "h264", Width: 3840, Height: 2160}, nil
		}
		return baichuan.StreamInfo{}, reset
	}
	var closed atomic.Bool
	rep := CameraReport{Streams: []StreamReport{
		streamReport("main", baichuan.StreamInfo{}, reset),
		streamReport("sub", baichuan.StreamInfo{}, reset),
		streamReport("extern", baichuan.StreamInfo{}, reset),
	}}
	login := closeFunc(func() { loginOpen.Store(false); closed.Store(true) })
	ok := retryMainAlone(context.Background(), login, "192.0.2.9:9000", baichuan.Options{}, &rep)
	if !ok || !rep.OneConnection || !closed.Load() {
		t.Fatalf("retry = %v, OneConnection = %v, login closed = %v", ok, rep.OneConnection, closed.Load())
	}
	if m := rep.Streams[0]; m.Undetermined || m.Width != 3840 {
		t.Fatalf("main = %+v, want it offered at 3840x2160", m)
	}
	for _, sr := range rep.Streams[1:] {
		if !sr.Undetermined || !strings.Contains(sr.Reason, "one connection") {
			t.Fatalf("%s = %+v, want it held back with the reason", sr.Name, sr)
		}
	}
	rec := httptest.NewRecorder()
	writeProbeJSON(rec, rep)
	if got := rec.Body.String(); !strings.Contains(got, `"name":"main"`) || strings.Contains(got, `"name":"sub"`) || !strings.Contains(got, "one connection at a time") {
		t.Fatalf("probe JSON = %s, want main alone with the note", got)
	}
}
