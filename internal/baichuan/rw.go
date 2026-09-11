package baichuan

import (
	"context"
	"fmt"
)

// StatusBadRequest is what a camera answers when it parsed the document and
// refused it, for instance an element name it does not know.
const StatusBadRequest = 400

// StatusWrongShape is the trap. A camera answers 421 when the message was
// built wrong, not when the feature is missing: a configuration write is two
// sections, the channel in an extension and the document in a second
// section, and sent as a single section every model answers 421 and changes
// nothing. The mis-numbered heartbeat produced the same 421 for a year while
// the feature it was aimed at was fine.
const StatusWrongShape = 421

// ReadConfig sends a config read that takes only a channel and waits for its
// reply, or for ctx to end.
func ReadConfig(ctx context.Context, conn *Conn, id uint32) ([]byte, int16, error) {
	if err := conn.GetConfig(id); err != nil {
		return nil, 0, err
	}
	return awaitReply(ctx, conn, id)
}

// WriteConfig sends body to id as a config write and reports the status the
// camera answered.
//
// A 200 from this function is not evidence the camera did anything. Message
// 47 and message 288 both accept a valid document, on hardware that
// implements the matching read, answer 200, and change nothing observable.
// Callers that need to know whether a write took effect must read the block
// back on a fresh connection and compare.
func WriteConfig(ctx context.Context, conn *Conn, id uint32, body []byte) (int16, error) {
	if len(body) == 0 {
		// An empty body is never correct and is how `get all` was
		// accidentally sending a user config set.
		return 0, fmt.Errorf("baichuan: refusing to write an empty body to message %d", id)
	}
	if err := conn.SetConfig(id, body); err != nil {
		return 0, err
	}
	_, status, err := awaitReply(ctx, conn, id)
	return status, err
}

// awaitReply waits for the reply carrying id. Anything else in flight, a
// ping reply for instance, is not the answer.
func awaitReply(ctx context.Context, conn *Conn, id uint32) ([]byte, int16, error) {
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				if err := conn.Err(); err != nil {
					return nil, 0, fmt.Errorf("baichuan: connection closed waiting for message %d: %w", id, err)
				}
				return nil, 0, fmt.Errorf("baichuan: connection closed waiting for message %d", id)
			}
			if m.Header.MsgID != id {
				continue
			}
			return m.XML, m.Header.Status(), nil
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// ExplainStatus turns a reply status into something an operator can act on.
// The wording matters more than it looks: 421 and 405 are routinely confused
// for each other, and treating 421 as "unsupported" is how a working feature
// gets written off.
func ExplainStatus(status int16) string {
	switch status {
	case 200:
		return "accepted (200). This is not proof the camera changed anything."
	case StatusBadRequest:
		return "refused (400). The camera parsed the document and rejected it, usually an element name it does not know."
	case StatusNotImplemented:
		return "this model does not implement that message (405)."
	case StatusWrongShape:
		return "the message was built wrong (421), not missing. A config write is two sections. This does not mean the model lacks the feature."
	default:
		return fmt.Sprintf("status %d.", status)
	}
}
