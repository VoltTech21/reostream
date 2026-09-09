package baichuan

import (
	"context"
	"testing"
	"time"

	"github.com/VoltTech21/reostream/internal/fakecam"
)

// A configuration write is a two section message: the channel in an
// extension, the document in a second section. Sent as a single section a
// camera answers 421 and changes nothing, which reads like an unsupported
// message rather than a malformed one, so this is worth pinning down.
//
// The shape shows in the header. PayloadOff is where the second section
// starts, so a two section message has MsgLen greater than PayloadOff, and a
// single section message has them equal.
func TestSetConfigWritesTwoSections(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const id = MsgIDOsdSet
	body := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n<body>\n<Osd/>\n</body>\n")
	if err := conn.SetConfig(id, body); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	// Give the write time to reach the camera's recording buffer.
	time.Sleep(100 * time.Millisecond)

	var found bool
	walkMessages(cam.Received(), func(h Header, _ []byte) bool {
		if h.MsgID != id {
			return true
		}
		found = true
		if h.PayloadOff == 0 {
			t.Errorf("PayloadOff is 0: the write carries no extension section")
		}
		if h.MsgLen <= h.PayloadOff {
			t.Errorf("MsgLen %d <= PayloadOff %d: the write is a single section, which a camera refuses with 421",
				h.MsgLen, h.PayloadOff)
		}
		return false
	})
	if !found {
		t.Fatalf("no message %d reached the camera", id)
	}
}

// The contrast: a read is one section, so the same two numbers are equal.
// If this ever diverges, the two paths have been conflated.
func TestGetConfigWritesOneSection(t *testing.T) {
	cam := fakecam.New(t, loadFixture(t, "h265_s2c.bin"))
	conn, err := Dial(context.Background(), cam.Addr(), Options{Password: ""})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const id = MsgIDGetSupport
	if err := conn.GetConfig(id); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	var found bool
	walkMessages(cam.Received(), func(h Header, _ []byte) bool {
		if h.MsgID != id {
			return true
		}
		found = true
		if h.MsgLen != h.PayloadOff {
			t.Errorf("MsgLen %d != PayloadOff %d: a read should carry one section",
				h.MsgLen, h.PayloadOff)
		}
		return false
	})
	if !found {
		t.Fatalf("no message %d reached the camera", id)
	}
}
