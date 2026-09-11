package camctl

import "github.com/VoltTech21/reostream/internal/baichuan"

// refused reports whether a message id must never be written, and why.
//
// This is not UnsafeToRewrite. That one flags writes which probably work
// and interrupt the stream doing it, so callers get a warning and the write
// still goes out. Everything here is refused outright, with no
// confirmation step, because a confirmation is not a safeguard against a
// mistake nobody can undo.
var refusals = map[uint32]string{
	// A firmware write is unrecoverable from a web page if it goes wrong.
	baichuan.MsgIDSetAutoUpdate: "firmware updates are not sent from this page",

	// A reboot takes the camera off the fleet for a minute, and this
	// daemon's whole discipline is about not disturbing cameras that are
	// carrying traffic.
	baichuan.MsgIDReboot: "a reboot takes the camera off the fleet; not sent from this page",

	// A factory reset is unrecoverable from a web page.
	baichuan.MsgIDRestore: "factory reset is not sent from this page",

	// Message 59 turned out to be a user config write sitting in this
	// project's read sweep: something was firing "Set user cfg" with an
	// empty body at live cameras, and nobody has established what that
	// does. A page that can rewrite accounts can lock an operator out of
	// their own camera with no way back short of a factory reset.
	baichuan.MsgIDSetUserCfg: "account writes can lock an operator out; not sent from this page",

	// Battery messages. None of 574/575 (sleep), 626/627 (battery mode),
	// 694/695 (PIR), or 687 (AOV) has ever been sent to a battery camera,
	// because there is not one on this fleet. Battery models sleep, wake
	// on motion, and send state messages this client has never parsed.
	// Assume broken means do not ship it.
	baichuan.MsgIDSetSleepStateCfg:      "battery camera message, never exercised on this fleet",
	baichuan.MsgIDGetSleepStateCfg:      "battery camera message, never exercised on this fleet",
	baichuan.MsgIDSetBatteryMode:        "battery camera message, never exercised on this fleet",
	baichuan.MsgIDGetBatteryMode:        "battery camera message, never exercised on this fleet",
	baichuan.MsgIDSetPirMotionDetectCfg: "battery camera message, never exercised on this fleet",
	baichuan.MsgIDGetPirMotionDetectCfg: "battery camera message, never exercised on this fleet",
	baichuan.MsgIDAovInfoReport:         "battery camera message, never exercised on this fleet",
}

// refused looks id up directly against the ids above rather than through
// ConfigPairs or ConfigMessages: ConfigPairs never pairs 58 and 59, because
// the firmware names them "get user cfg" and "Set user cfg" and the pairing
// is case sensitive, so the account write is invisible to it. A refusal
// list built on ConfigPairs would inherit that blind spot.
func refused(id uint32) (bool, string) {
	why, ok := refusals[id]
	return ok, why
}
