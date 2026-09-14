package control

import (
	"context"
	"testing"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// TestRefusedMessagesAreNeverWritable checks refused() against the exported
// message id constants directly, not against baichuan.ConfigMessages: that
// map only names the read requests, so "reboot", "restore", "set auto
// update" and "Set user cfg" are never keys in it and a lookup through it
// would silently skip every case here. Each id is instead verified against
// baichuan.MsgName so the test fails loudly if a constant ever stops
// meaning what its comment says, rather than passing by never checking
// anything.
func TestRefusedMessagesAreNeverWritable(t *testing.T) {
	// Each of these is refused for its own reason and none is behind a
	// confirmation, because a confirmation is not a safeguard against a
	// mistake nobody can undo.
	cases := []struct {
		id   uint32
		name string // the firmware's exact description, capitalisation included
	}{
		{baichuan.MsgIDSetAutoUpdate, "set auto update"}, // firmware
		{baichuan.MsgIDReboot, "reboot"},                 // takes the camera off the fleet for a minute
		{baichuan.MsgIDRestore, "restore"},               // factory reset
		{baichuan.MsgIDSetUserCfg, "Set user cfg"},       // can lock an operator out of their own camera
	}

	resolved := 0
	for _, c := range cases {
		if got := baichuan.MsgName(c.id); got != c.name {
			t.Fatalf("id %d is named %q by the firmware table, want %q", c.id, got, c.name)
		}
		resolved++

		yes, why := refused(c.id)
		if !yes {
			t.Fatalf("%s (%d) is writable", c.name, c.id)
		}
		if why == "" {
			t.Fatalf("%s is refused with no reason given", c.name)
		}
	}
	if resolved == 0 {
		t.Fatal("no case resolved against the firmware name table; this test checked nothing")
	}
}

// TestBatteryMessagesAreRefused checks refused() against every known
// battery message id. It counts how many it actually checked and fails if
// that count is zero, so a typo'd id list cannot pass this test by
// accident.
func TestBatteryMessagesAreRefused(t *testing.T) {
	// The ids are known: 574/575 sleep, 626/627 battery mode, 694/695 PIR,
	// 687 AOV. None has ever been sent to a battery camera, because there
	// is not one here. They sleep, wake on motion, and send state messages
	// this client has never parsed. Assume broken means do not ship it.
	ids := []uint32{574, 575, 626, 627, 694, 695, 687}
	checked := 0
	for _, id := range ids {
		checked++
		yes, why := refused(id)
		if !yes {
			t.Fatalf("battery message %d is writable", id)
		}
		if why == "" {
			t.Fatalf("battery message %d is refused with no reason given", id)
		}
	}
	if checked == 0 {
		t.Fatal("no battery message id was checked; this test checked nothing")
	}
}

// TestWriteBlockHonoursTheRefusal proves the guard lives in the write
// path, not only in whatever UI builds the write form. It calls
// s.writeBlock directly against a refused id, using a camera address from
// the documentation range (see writeTestConfig): if refused() were only
// consulted by a template, writeBlock would try to dial that unreachable
// address and this test would hang until probeTimeout instead of
// returning immediately with outcome "refused".
func TestWriteBlockHonoursTheRefusal(t *testing.T) {
	s := newTestServer(t, Options{Password: "hunter2", ConfigPath: writeTestConfig(t, "cam1")})
	cam, err := s.byName("cam1")
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.writeBlock(context.Background(), cam, baichuan.MsgIDReboot, []byte("<x/>"), false)
	if err != nil {
		t.Fatalf("writeBlock returned an error instead of a refusal: %v", err)
	}
	if result.Outcome != "refused" {
		t.Fatalf("outcome %q, want %q", result.Outcome, "refused")
	}
	if result.Detail == "" {
		t.Fatal("refused with no reason given")
	}
}
