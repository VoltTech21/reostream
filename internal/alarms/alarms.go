// Package alarms follows a camera's own detection state: it holds a
// Baichuan session subscribed to alarm events (message 31, then 33 pushes)
// and reports each change -- motion starting or stopping, an object type
// appearing or going away. It only listens; nothing on the camera changes.
package alarms

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// State is what a camera currently reports.
type State struct {
	Motion bool
	// Objects are the AI types the camera names, normalized: person,
	// vehicle, animal, face, package, visitor.
	Objects []string
}

// Change is one transition.
type Change struct {
	Camera string
	Time   time.Time
	// Kind is "motion" or an object type; On says it started.
	Kind string
	On   bool
}

func normalize(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "people", "person", "human":
		return "person"
	case "vehicle", "car":
		return "vehicle"
	case "dog_cat", "pet", "animal":
		return "animal"
	case "face":
		return "face"
	case "package":
		return "package"
	case "visitor":
		return "visitor"
	case "", "none":
		return ""
	}
	return strings.ToLower(t)
}

// stateOf merges every channel's event into one camera state (a TrackMix
// reports its two lenses as channels 0 and 1).
func stateOf(events []baichuan.AlarmEvent) State {
	var s State
	seen := map[string]bool{}
	for _, e := range events {
		if e.Motion() {
			s.Motion = true
		}
		if e.Visitor() && !seen["visitor"] {
			seen["visitor"] = true
			s.Objects = append(s.Objects, "visitor")
		}
		for _, t := range e.AI {
			if n := normalize(t); n != "" && !seen[n] {
				seen[n] = true
				s.Objects = append(s.Objects, n)
			}
		}
	}
	sort.Strings(s.Objects)
	return s
}

// diff lists the changes from a to b.
func diff(camera string, now time.Time, a, b State) []Change {
	var out []Change
	if a.Motion != b.Motion {
		out = append(out, Change{camera, now, "motion", b.Motion})
	}
	had, has := map[string]bool{}, map[string]bool{}
	for _, o := range a.Objects {
		had[o] = true
	}
	for _, o := range b.Objects {
		has[o] = true
		if !had[o] {
			out = append(out, Change{camera, now, o, true})
		}
	}
	for _, o := range a.Objects {
		if !has[o] {
			out = append(out, Change{camera, now, o, false})
		}
	}
	return out
}

// Watch follows one camera until ctx ends, reconnecting with backoff, and
// calls fn for every change. A lost connection ends whatever was on, so a
// listener is never left believing motion continues.
func Watch(ctx context.Context, camera string, dial func(context.Context) (*baichuan.Conn, error), fn func(Change)) {
	var cur State
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := follow(ctx, camera, dial, &cur, fn)
		for _, c := range diff(camera, time.Now(), cur, State{}) {
			fn(c)
		}
		cur = State{}
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
		}
		log.Printf("alarms: %s: %v; again in %s", camera, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func follow(ctx context.Context, camera string, dial func(context.Context) (*baichuan.Conn, error), cur *State, fn func(Change)) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, err := dial(dctx)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = conn.StartAlarms(sctx)
	cancel()
	if err != nil {
		return err
	}
	beat := time.NewTicker(5 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-beat.C:
			if err := conn.HeartBeat(); err != nil {
				return err
			}
		case m, ok := <-conn.Messages():
			if !ok {
				return conn.Err()
			}
			if !baichuan.IsAlarmEvent(m) {
				continue
			}
			events, err := baichuan.ParseAlarmEvents(m.XML)
			if err != nil {
				continue
			}
			// A connection opened for one channel of an NVR or Home Hub
			// still hears every channel's events; keep this camera's. A
			// recorder calls itself something other than "ipc" at login,
			// which is what tells its channel 0 from a camera's own.
			if ch := conn.Channel(); ch != 0 || isRecorder(conn) {
				kept := events[:0]
				for _, e := range events {
					if e.Channel == ch {
						kept = append(kept, e)
					}
				}
				events = kept
			}
			next := stateOf(events)
			for _, c := range diff(camera, time.Now(), *cur, next) {
				fn(c)
			}
			*cur = next
		}
	}
}

func isRecorder(conn *baichuan.Conn) bool {
	di, err := baichuan.ParseDeviceInfo(conn.DeviceInfo())
	if err != nil {
		return false
	}
	// Cameras say "ipc", "fisheye_ipc" and the like, with typeInfo "IPC";
	// a recorder says neither.
	t := strings.ToLower(di.DeviceInfo.Type + " " + di.DeviceInfo.TypeInfo)
	return strings.TrimSpace(t) != "" && !strings.Contains(t, "ipc")
}
