package bcudp

import (
	"net"
	"sync"
	"time"
)

// A battery camera's search answer drops its battery block for any
// client session, ours included, and does not say whose. So every session
// this process opens is noted here, from the first packet of the dial to
// its close, and a camera with a session going while one of ours was open
// (or had just closed) is awake for us.
var ours struct {
	mu   sync.Mutex
	open map[string]int
	last map[string]time.Time
}

func oursOpen(ip net.IP) {
	k := ip.String()
	ours.mu.Lock()
	defer ours.mu.Unlock()
	if ours.open == nil {
		ours.open = map[string]int{}
		ours.last = map[string]time.Time{}
	}
	ours.open[k]++
	ours.last[k] = time.Now()
}

func oursClosed(ip net.IP) {
	k := ip.String()
	ours.mu.Lock()
	defer ours.mu.Unlock()
	if ours.open[k] > 0 {
		ours.open[k]--
	}
	ours.last[k] = time.Now()
}

// Ours reports whether this process has a session with the camera at ip
// open, or had one within the last d: any other session is someone else's
// (the Reolink app, another reostream). Motion is not a session at all; it
// shows in how fast the camera answers (see the supervisor's wake watch).
func Ours(ip net.IP, d time.Duration) bool {
	k := ip.String()
	ours.mu.Lock()
	defer ours.mu.Unlock()
	return ours.open[k] > 0 || time.Since(ours.last[k]) < d
}
