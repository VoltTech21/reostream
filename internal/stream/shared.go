package stream

import (
	"sync"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// source is what Run reads a stream through: a camera's own connection, or
// one reader on a connection an NVR's streams share.
type source interface {
	Messages() <-chan baichuan.Message
	Err() error
	DeviceInfo() []byte
	Ping() error
}

var (
	poolMu sync.Mutex
	pool   = baichuan.NewPool()
)

// SetRecorderSharing sets how many streams one connection to an NVR or
// Home Hub carries (baichuan.Pool.MaxPreviews). Zero gives every stream a
// connection of its own, as every release before this one did; negative
// leaves the default. Call it before any stream starts.
func SetRecorderSharing(perConnection int) {
	poolMu.Lock()
	defer poolMu.Unlock()
	switch {
	case perConnection == 0:
		pool = nil
	case perConnection > 0:
		pool = baichuan.NewPool()
		pool.MaxPreviews = perConnection
	}
}

func currentPool() *baichuan.Pool {
	poolMu.Lock()
	defer poolMu.Unlock()
	return pool
}
