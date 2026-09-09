package stream

import (
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
	"github.com/VoltTech21/reostream/internal/fakecam"
	"github.com/VoltTech21/reostream/internal/hub"
	"github.com/VoltTech21/reostream/internal/ts"
)

// isHEVCIRAPType reports whether an HEVC NAL unit type is one of the
// intra-random-access-point types (BLA, IDR or CRA): the types that start a
// GOP and carry no references to earlier frames. baichuan.FrameIFrame is
// keyed off the wire's own frame-type byte, not the NAL type, so this is an
// independent check on the actual encoded bytes, not a restatement of what
// produced them.
func isHEVCIRAPType(nalType byte) bool {
	return nalType >= 16 && nalType <= 21
}

// videoAccessUnitNALTypes walks chunk as whole 188 byte transport packets,
// reassembles the first video access unit found (from its PES start packet
// through every following PIDVideo payload packet, the same span
// ts.Muxer.packetisePES produced it from), and returns every HEVC NAL type
// present in it.
//
// This is a direct MPEG-TS/PES parse, not a call into internal/ts: the
// point of this test is to check what actually went out on the wire against
// an independent reading of it, not against the package under test's own
// idea of what it wrote. An HEVC keyframe access unit carries VPS (32),
// SPS (33) and PPS (34) ahead of its IDR or CRA slice, so the type that
// proves a keyframe is not necessarily the first NAL in the unit; every
// type present has to be checked.
func videoAccessUnitNALTypes(t *testing.T, chunk []byte) []byte {
	t.Helper()
	var es []byte
	started := false
	for off := 0; off+ts.PacketSize <= len(chunk); off += ts.PacketSize {
		pkt := chunk[off : off+ts.PacketSize]
		if pkt[0] != 0x47 {
			t.Fatalf("chunk is not packet aligned: byte %d is %#x, not a sync byte", off, pkt[0])
		}
		pid := ts.PID(pkt[1]&0x1F)<<8 | ts.PID(pkt[2])
		if pid != ts.PIDVideo {
			continue
		}
		payloadStart := pkt[1]&0x40 != 0
		afc := (pkt[3] >> 4) & 0x3
		if afc == 2 {
			continue // adaptation field only: the PCR packet, no payload
		}
		i := 4
		if afc == 3 {
			i = 5 + int(pkt[4]) // adaptation_field_length excludes itself
		}
		payload := pkt[i:]
		switch {
		case !started && payloadStart:
			if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
				continue // not a PES start, e.g. a table repeat sharing this chunk
			}
			started = true
			headerDataLen := int(payload[8])
			es = append(es, payload[9+headerDataLen:]...)
		case started && !payloadStart:
			es = append(es, payload...)
		case started && payloadStart:
			// A second PES start on PIDVideo means the first access unit's
			// packets, including its adaptation-field stuffing on the
			// last one, have all been consumed.
			return nalTypes(es)
		}
	}
	return nalTypes(es)
}

// nalTypes extracts every HEVC NAL unit type from an Annex B byte stream.
func nalTypes(es []byte) []byte {
	var types []byte
	for j := 0; j+4 <= len(es); j++ {
		var hdr int
		switch {
		case es[j] == 0 && es[j+1] == 0 && es[j+2] == 0 && es[j+3] == 1:
			hdr = j + 4
		case es[j] == 0 && es[j+1] == 0 && es[j+2] == 1:
			hdr = j + 3
		default:
			continue
		}
		if hdr >= len(es) {
			break
		}
		types = append(types, (es[hdr]>>1)&0x3F)
	}
	return types
}

// TestLateSubscriberFirstBytesBeginAtAKeyframe reproduces, end to end, the
// gap measured against a live camera on 2026-09-08: a client that joins
// mid GOP saw slices before the parameter sets describing them, and its
// decoder logged about 9 groups of reference errors in the first 60
// seconds before recovering at the next keyframe.
//
// This drives the real pipeline, baichuan.Dial through a fakecam, the real
// depacketiser, and the real ts.Muxer, but does it single threaded rather
// than through stream.Run: the fixture has no real time pacing (fakecam
// hands the whole capture to the kernel in one Write), so a second
// goroutine racing to subscribe "mid stream" would not reliably land
// there. Driving the frame loop by hand lets the test choose the exact
// mid-GOP point deterministically, which is what "subscribe partway
// through the stream, not at the start" requires. The fixture has 4 I
// frames and 85 P frames (see internal/baichuan/testdata/README.md's
// h265_s2c.bin); this subscribes after the second I frame's GOP is well
// under way and confirms the late subscriber's first delivered chunk
// starts a fresh IRAP NAL, not a P frame's slice.
func TestLateSubscriberFirstBytesBeginAtAKeyframe(t *testing.T) {
	cam := fakecam.New(t, h265Fixture(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := baichuan.Dial(ctx, cam.Addr(), baichuan.Options{Username: "admin", Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.StartVideo(baichuan.StreamMain); err != nil {
		t.Fatalf("start video: %v", err)
	}

	h := hub.New(256)
	d := baichuan.NewDepacketiser()
	var mux *ts.Muxer
	var iframes int
	var late <-chan []byte
	var cancelLate func()

	// fakecam holds the connection open in silence once the fixture is
	// exhausted (see fakecam.New's doc: it never closes on its own), the
	// same as a real camera idling with nothing new to say. So this loop
	// cannot wait for conn.Messages() to close; it stops itself once the
	// third I frame has gone out, which is the keyframe the late
	// subscriber, joined mid way through the second GOP, is waiting for.
	done := false
	timeout := time.After(10 * time.Second)
loop:
	for {
		select {
		case <-timeout:
			t.Fatal("timed out reading the fixture")
		case msg, open := <-conn.Messages():
			if !open {
				break loop
			}
			if len(msg.Payload) == 0 {
				continue
			}
			d.Write(msg.Payload, msg.StartsPacket)
			for {
				f, ok := d.Next()
				if !ok {
					break
				}
				switch f.Kind {
				case baichuan.FrameIFrame, baichuan.FramePFrame:
					if mux == nil {
						nm, err := ts.NewMuxerWithAudio(f.Codec, "aac")
						if err != nil {
							t.Fatalf("new muxer: %v", err)
						}
						mux = nm
						h.SetHeader(mux.Header())
					}
					if f.Kind == baichuan.FrameIFrame {
						iframes++
					}
					if pkt, key := mux.FrameWithKey(f); pkt != nil {
						h.PublishKey(pkt, key)
					}
					// Subscribe the late listener after the second GOP has
					// clearly started: past its own I frame (iframes==2)
					// and several P frames into it. This is genuinely mid
					// GOP, not a race against real time.
					if late == nil && iframes == 2 && f.Kind == baichuan.FramePFrame {
						late, cancelLate = h.Subscribe()
						defer cancelLate()
					}
					if iframes == 3 {
						// The third I frame is the keyframe the late
						// subscriber has been waiting for. Nothing past
						// this point is needed to answer the question this
						// test asks.
						done = true
					}
				case baichuan.FrameAAC, baichuan.FrameADPCM:
					if mux != nil {
						if pkt, _ := mux.FrameWithKey(f); pkt != nil {
							h.Publish(pkt)
						}
					}
				}
			}
			if done {
				break loop
			}
		}
	}

	if late == nil {
		t.Fatal("never reached the point in the fixture where the late subscriber joins; fixture changed?")
	}

	var first []byte
	select {
	case chunk, open := <-late:
		if !open {
			t.Fatal("late subscriber's channel closed with nothing delivered")
		}
		first = chunk
	default:
		t.Fatal("late subscriber received nothing, even though the rest of the fixture already published")
	}

	types := videoAccessUnitNALTypes(t, first)
	if len(types) == 0 {
		t.Fatalf("first chunk delivered to the late subscriber carries no video PES start: %x", first)
	}
	var sawIRAP bool
	for _, nt := range types {
		if isHEVCIRAPType(nt) {
			sawIRAP = true
		}
	}
	if !sawIRAP {
		t.Fatalf("first video access unit delivered to the late subscriber has NAL types %v, want an IRAP type "+
			"(16-21) among them: a late joiner must not see a P frame's slice before any keyframe", types)
	}
}
