package baichuan

import (
	"context"
	"fmt"
)

// StatusNotImplemented is the status a camera answers for a config message
// it does not implement, instead of failing the connection. docs/control.md
// establishes this is what makes probing every known read safe.
const StatusNotImplemented = 405

// requestConfig sends a config read that takes only a channel and waits for
// its reply, or for ctx to end.
//
// This is cmd/reocam's fetch, lifted here so the CLI and anything else in
// this module share one wait loop instead of each keeping its own copy that
// can drift: cmd/reocam's own get, support and probe commands all called an
// identical unexported fetch before this existed. It now delegates to
// ReadConfig (rw.go) rather than keeping its own copy of the same loop.
func requestConfig(ctx context.Context, conn *Conn, id uint32) ([]byte, int16, error) {
	return ReadConfig(ctx, conn, id)
}

// GetSupport reads the camera's own hardware description, message 199 ("get
// support"). This is the read cmd/reocam's support command has always used,
// lifted here rather than duplicated so the CLI and anything else in this
// module cannot answer the question two different ways.
func GetSupport(ctx context.Context, conn *Conn) (Support, error) {
	x, status, err := requestConfig(ctx, conn, ConfigMessages["support"])
	if err != nil {
		return Support{}, err
	}
	if len(x) == 0 {
		if status == StatusNotImplemented {
			return Support{}, fmt.Errorf("baichuan: this camera does not answer the support read")
		}
		return Support{}, fmt.Errorf("baichuan: support read answered status %d with no body", status)
	}
	return ParseSupport(x)
}

// GetAbilities reads what the logged in user may do, module by module
// (message 151, AbilityInfo). This is the read cmd/reocam's abilities
// command has always used.
func GetAbilities(ctx context.Context, conn *Conn) ([]Ability, error) {
	if err := conn.Abilities(); err != nil {
		return nil, err
	}
	for {
		select {
		case m, ok := <-conn.Messages():
			if !ok {
				if err := conn.Err(); err != nil {
					return nil, fmt.Errorf("baichuan: connection closed waiting for an ability reply: %w", err)
				}
				return nil, fmt.Errorf("baichuan: connection closed waiting for an ability reply")
			}
			if m.Header.MsgID != MsgIDAbilityInfo || len(m.XML) == 0 {
				continue
			}
			return ParseAbilities(m.XML)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
