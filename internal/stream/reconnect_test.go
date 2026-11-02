package stream

import (
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// tsPacket is what the reconnect test reads back out of one transport
// packet.
type tsPacket struct {
	pid  ts.PID
	cc   byte
	pts  int64 // -1 when the packet starts no video PES
	disc bool  // discontinuity_indicator
}

func parseTS(t *testing.T, b []byte) []tsPacket {
	t.Helper()
	var out []tsPacket
	for ; len(b) >= ts.PacketSize; b = b[ts.PacketSize:] {
		p := b[:ts.PacketSize]
		if p[0] != 0x47 {
			t.Fatal("lost packet sync")
		}
		pk := tsPacket{pid: ts.PID(p[1]&0x1F)<<8 | ts.PID(p[2]), cc: p[3] & 0x0F, pts: -1}
		off := 4
		if p[3]&0x20 != 0 {
			if p[4] > 0 {
				pk.disc = p[5]&0x80 != 0
			}
			off += 1 + int(p[4])
		}
		if p[1]&0x40 != 0 && pk.pid == ts.PIDVideo && off+14 <= len(p) && p[off] == 0 && p[off+1] == 0 && p[off+2] == 1 {
			f := p[off+9:]
			pk.pts = int64(f[0]>>1&0x07)<<30 | int64(f[1])<<22 | int64(f[2]>>1)<<15 | int64(f[3])<<7 | int64(f[4]>>1)
		}
		out = append(out, pk)
	}
	return out
}

// The HTTP .ts keeps its timeline over a camera reconnect: PTS keeps
// increasing and every PID's continuity counter keeps counting, where a new
// muxer per connection started both over at zero (a link-drop test, 10-30:
// go2rtc then made Frigate's recordings unusable for as long as the
// previous connection had lasted).
func TestTheTSTimelineContinuesAcrossAReconnect(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))
	h := hub.New(100000)
	ch, cancelSub := h.Subscribe()
	defer cancelSub()
	cfg := Config{Name: "test", Address: cam.Addr(), Username: "admin", Stream: "main"}

	var all []byte
	for conn := 0; conn < 2; conn++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg, h) }()
		// The fixture holds a few seconds of video; take what arrives in
		// a moment, then drop the connection.
		quiet := time.NewTimer(500 * time.Millisecond)
	read:
		for {
			select {
			case c := <-ch:
				all = append(all, c...)
				quiet.Reset(500 * time.Millisecond)
			case <-quiet.C:
				break read
			case <-time.After(5 * time.Second):
				t.Fatal("no data")
			}
		}
		cancel()
		<-done
		time.Sleep(200 * time.Millisecond)
	}

	pkts := parseTS(t, all)
	var last int64 = -1
	resets, ptsCount := 0, 0
	cc := map[ts.PID]int{}
	for _, p := range pkts {
		if p.pts >= 0 {
			ptsCount++
			if p.pts < last {
				t.Errorf("PTS went back from %d to %d", last, p.pts)
			}
			if last > 90000 && p.pts < 90000 {
				resets++
			}
			last = p.pts
		}
		if prev, ok := cc[p.pid]; ok && byte(prev+1)&0x0F != p.cc && !p.disc {
			t.Errorf("PID %#x: continuity counter %d after %d", p.pid, p.cc, prev)
		}
		cc[p.pid] = int(p.cc)
	}
	if ptsCount < 20 {
		t.Fatalf("only %d video frames over two connections", ptsCount)
	}
	if resets > 0 {
		t.Errorf("the timeline started over %d times", resets)
	}
}

// A camera that comes back with another codec gets a new muxer that still
// continues the timeline, says so with the discontinuity indicator, and
// a new program map version.
func TestRecodecContinuesTheTimeline(t *testing.T) {
	o := &tsOutput{}
	start := time.Now()
	m1, changed, err := o.muxer("H265", true, start)
	if err != nil || !changed {
		t.Fatal(err, changed)
	}
	o.connected()
	m2, changed, err := o.muxer("H265", true, start.Add(time.Second))
	if err != nil || changed || m2 != m1 {
		t.Fatalf("same codec: a new muxer (changed %v, same %v)", changed, m2 == m1)
	}
	o.connected()
	m3, changed, err := o.muxer("H264", true, start.Add(2*time.Second))
	if err != nil || !changed || m3 == m1 {
		t.Fatalf("another codec: changed %v, err %v", changed, err)
	}
	if !m3.Carries("h264", true) {
		t.Error("the new muxer is not H.264 with audio")
	}
	if v := m3.Header()[ts.PacketSize+10] >> 1 & 0x1F; v != 1 {
		t.Errorf("program map version %d after a codec change, want 1", v)
	}
}
