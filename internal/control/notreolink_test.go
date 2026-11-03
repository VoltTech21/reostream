package control

import (
	"context"
	"net"
	"testing"
	"time"
)

// A Google speaker has port 9000 open too (janoski99, 11-02). Whatever
// answers in another protocol is not offered as a camera.
func TestProbeMarksADeviceThatIsNotAReolink(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			c.Read(buf)
			c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep := probeCamera(ctx, ln.Addr().String(), "admin", "")
	if !rep.NotReolink {
		t.Fatalf("an HTTP answer was not marked as not a Reolink: %+v", rep)
	}
}
