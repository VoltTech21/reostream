package control

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// WriteResult is what actually happened when this codebase tried to change
// one block, as distinct from what the camera claimed.
//
// Outcome is one of three words, and the words carry the whole point of
// this file:
//
//   - "confirmed": the write was made, the block was read back on a fresh
//     connection, and the field holds the new value.
//   - "accepted": the camera answered 200. No claim is made about effect.
//     Message 47 (md set) and message 288 (floodlight set) both do this on
//     models that implement the matching read, and change nothing.
//   - "refused": the camera rejected the write. Detail explains what the
//     status actually means, because 421 reads like "not supported" and
//     means the message was built wrong instead.
//
// Before is the document this block held before the write, captured on the
// same connection the write went out on, so a caller can offer to restore
// it byte identical. After is the document actually observed afterward: the
// fresh-connection read-back when one was taken, empty otherwise. It is
// never the body this codebase merely sent, because what was sent is not
// evidence of what is now on the camera.
type WriteResult struct {
	Outcome string
	Detail  string
	Before  []byte
	After   []byte
}

// outcomeFor maps a write's status and, when verified, its fresh-connection
// read-back, onto the three outcomes. It does not touch the network: every
// dial and read happens in writeBlock, so this stays a pure function a test
// can drive with fixed bytes rather than a fake camera.
//
// The confirmed check is bytes.Contains(readBack, wrote): wrote is the
// whole document this codebase sent, not just the one field a caller
// meant to change, and outcomeFor has no xpath or field name to compare
// against on its own, only the two full documents. So this asks the
// stricter question "does the fresh read-back carry the exact document
// this codebase sent, byte for byte", not "does the changed field hold the
// new value": a camera that reformats whitespace, reorders siblings, or
// renumbers an attribute elsewhere in the document fails this check even
// though the field itself took, and reads as accepted rather than
// confirmed. That is the safe direction to err in, since confirmed is the
// stronger claim, but it does mean confirmed will rarely fire against a
// camera that reformats.
func outcomeFor(status int16, wrote, readBack []byte, verify bool) WriteResult {
	if status != 200 {
		return WriteResult{Outcome: "refused", Detail: baichuan.ExplainStatus(status)}
	}
	if verify && bytes.Contains(readBack, wrote) {
		return WriteResult{
			Outcome: "confirmed",
			Detail:  "confirmed: read back on a fresh connection, and the document sent is present in it byte for byte.",
		}
	}
	// Either verify was never asked for, or it was and the read-back does
	// not carry what was written: a 200 the camera answered that changed
	// nothing observable looks exactly like this. Both cases get the same
	// honest word.
	return WriteResult{Outcome: "accepted", Detail: baichuan.ExplainStatus(status)}
}

// pairForSet finds the read/write pair whose Set id is id. Get and Set are
// different message ids on the wire (osd get is 44, osd set is 45), so a
// document cannot be read back by asking for the Set id again; the paired
// Get id is the only message that answers with the document at all.
func pairForSet(id uint32) (baichuan.ConfigPair, bool) {
	for _, p := range baichuan.ConfigPairs() {
		if p.Set == id {
			return p, true
		}
	}
	return baichuan.ConfigPair{}, false
}

// writeBlock reads the pair's Get id first, on the connection it is about
// to write to, so Before is captured before anything changes, writes body
// to id (the pair's Set id), and, when verify is set and the camera
// answered 200, dials a fresh connection and reads the Get id back.
//
// The fresh connection is not incidental. A read on the same connection can
// be answered from state the camera has not committed yet, so reusing the
// write connection to "verify" would quietly turn confirmed into a claim
// this codebase has no right to make. Every effect docs/control.md records
// as proven was confirmed on a connection opened after the write, not the
// one that carried it.
func (s *Server) writeBlock(ctx context.Context, cam Camera, id uint32, body []byte, verify bool) (result WriteResult, err error) {
	// refused is checked before anything else here, not only in the UI
	// that builds the write form, so it holds even for a route added
	// later by someone who has not read why these ids are refused.
	if yes, why := refused(id); yes {
		return WriteResult{Outcome: "refused", Detail: why}, nil
	}

	pair, ok := pairForSet(id)
	if !ok {
		return WriteResult{}, fmt.Errorf("control: %d is not a known writable pair for %q", id, cam.Name)
	}

	// UnsafeToRewrite is about this codebase's own evidence, from
	// docs/control.md, not the status the camera answers: a clean 200 on a
	// pair known to reconfigure the pipeline gives no hint by itself that
	// the stream was just interrupted, and that is exactly the case an
	// operator most needs the warning for. This defer is the one place
	// that appends it, running after every branch below has finished
	// deciding Detail, including the two verification-failure branches
	// that replace Detail outright with their own message: appending
	// per-branch, as an earlier version of this function did, meant a
	// later branch's plain assignment silently discarded a warning an
	// earlier branch had already added. A single append at the end cannot
	// be clobbered by branches that run before it.
	defer func() {
		if err == nil && confidenceOf(pair) == UnsafeToRewrite {
			result.Detail = "this pair is unsafe to rewrite: re-applying it interrupts the stream, even though the write itself succeeded. " + result.Detail
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: writing %s for %q: %w", pair.Name, cam.Name, err)
	}
	// conn is reassigned below when verify redials onto a fresh connection,
	// so this cannot be `defer conn.Close()`: that binds to whatever conn
	// holds right now and would go on closing the stale, already-replaced
	// connection instead of the live one. A closure re-reads conn at
	// return time, so whichever connection is current gets closed, and
	// only that one. An unclosed connection holds the camera's session
	// open and the camera refuses new connections for minutes.
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
	before, _, err := baichuan.ReadConfig(readCtx, conn, pair.Get)
	readCancel()
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: reading %s before writing for %q: %w", pair.Name, cam.Name, err)
	}

	writeCtx, writeCancel := context.WithTimeout(ctx, readTimeout)
	status, err := baichuan.WriteConfig(writeCtx, conn, id, body)
	writeCancel()
	if err != nil {
		return WriteResult{}, fmt.Errorf("control: writing %s for %q: %w", pair.Name, cam.Name, err)
	}

	result = outcomeFor(status, body, nil, false)
	result.Before = before

	if status != 200 || !verify {
		return result, nil
	}

	// Close the write connection explicitly and dial a fresh one for the
	// read-back, rather than reusing conn: see the function comment. conn
	// is reassigned here, which is exactly the case the deferred closure
	// above exists for.
	conn.Close()
	conn = nil
	fresh, dialErr := s.dial(ctx, cam)
	if dialErr != nil {
		// The write itself is not in doubt, only whether it can be proven.
		// Report what was actually established: a 200 with no completed
		// verification is accepted, not confirmed.
		result.Detail = fmt.Sprintf("accepted (200), but the fresh connection for verification could not be opened: %v. This is not proof the camera changed anything.", dialErr)
		return result, nil
	}
	conn = fresh

	readCtx, readCancel = context.WithTimeout(ctx, readTimeout)
	after, _, readErr := baichuan.ReadConfig(readCtx, conn, pair.Get)
	readCancel()
	if readErr != nil {
		result.Detail = fmt.Sprintf("accepted (200), but the read-back on a fresh connection failed: %v. This is not proof the camera changed anything.", readErr)
		return result, nil
	}

	result = outcomeFor(status, body, after, true)
	result.Before = before
	result.After = after
	return result, nil
}

// writeResultPage is what result.html renders after a write from the raw
// block editor, and only from there.
//
// It used to be what all four write forms rendered. The other three -- a
// curated setting, the floodlight, the NTP form -- now redirect back to
// the page the setting lives on and report in one line; see flash.go. The
// raw editor keeps this page because seeing the exact before and after XML
// is the entire point of the Advanced page: an operator there is checking
// what a camera did to a document byte for byte, which is not something a
// banner can carry.
type writeResultPage struct {
	Title  string
	Camera Camera

	Outcome string
	Detail  string
	Before  string
	After   string

	// RestoreAction, when non-empty, shows a form that resubmits
	// RestoreValue as RestoreParam through the same verified write path a
	// person used to get here: this is how "Before" stops being a value
	// only the JSON carried and becomes a value an operator can actually
	// put back.
	//
	// A blank RestoreAction is not a bug in every case: the floodlight and
	// NTP forms do not always have a document to restore in the same
	// shape a resubmission needs, and offering a button that cannot
	// actually restore anything would be worse than offering none.
	RestoreAction string
	RestoreParam  string
	RestoreValue  string
	// RestoreHidden carries any other fixed field the restore POST needs
	// beyond RestoreParam/RestoreValue, such as verify=true for a
	// document write through /write/{id}.
	RestoreHidden map[string]string
}

// serveWrite writes one config block on one camera and reports what
// actually happened. It is also how a restore runs: a caller resubmits the
// Before bytes an earlier write returned, and it goes through this same
// path, which is what gets it verified the same way an ordinary write is.
func (s *Server) serveWrite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	id64, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		http.Error(w, "bad message id", http.StatusBadRequest)
		return
	}
	id := uint32(id64)

	// Only a message this codebase actually knows as the Set half of a
	// read/write pair may be written. A request for anything else is not a
	// message id this page has ever read a document from, and composing a
	// write to a message nobody has confirmed the shape of is exactly the
	// kind of thing that produces a 421 or worse.
	if _, ok := pairForSet(id); !ok {
		http.Error(w, fmt.Sprintf("message %d is not a known writable pair", id), http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.FormValue("body")
	if body == "" {
		http.Error(w, "an empty body is never correct", http.StatusBadRequest)
		return
	}
	verify := r.FormValue("verify") != "false"

	result, err := s.writeBlock(r.Context(), cam, id, []byte(body), verify)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	page := writeResultPage{
		Title:   fmt.Sprintf("%s: write %d", cam.Name, id),
		Camera:  cam,
		Outcome: result.Outcome,
		Detail:  result.Detail,
		Before:  string(result.Before),
		After:   string(result.After),
	}
	if len(result.Before) > 0 {
		page.RestoreAction = fmt.Sprintf("/cameras/%s/write/%d", cam.Name, id)
		page.RestoreParam = "body"
		page.RestoreValue = string(result.Before)
		page.RestoreHidden = map[string]string{"verify": "true"}
	}
	s.render(w, "result.html", page)
}
