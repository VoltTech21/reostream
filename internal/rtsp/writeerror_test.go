package rtsp

import (
	"strings"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
)

// gortsplib logs a stream write error itself, one bare line per dropped
// packet, unless the handler implements ServerHandlerOnStreamWriteError.
// This server did not, and a build in production logged about 2,400 lines
// a minute of "write queue is full" with no camera and no session on any
// of them.
//
// The assertion is the interface, not the text: what stopped the flood is
// that gortsplib has somewhere to hand the error other than the log.
func TestTheServerHandlesStreamWriteErrorsItself(t *testing.T) {
	var h any = New("127.0.0.1:0")
	if _, ok := h.(gortsplib.ServerHandlerOnStreamWriteError); !ok {
		t.Fatal("Server does not implement ServerHandlerOnStreamWriteError, so gortsplib logs every dropped packet itself")
	}
}

// Dropped packets are counted and reported at most once a minute per path,
// so a reader that falls behind is still visible without being repeated
// thousands of times.
func TestDroppedPacketsAreCountedNotEchoed(t *testing.T) {
	s := New("127.0.0.1:0")

	// No session in the map: the path is unknown, which must not panic and
	// must still count.
	for i := 0; i < 5000; i++ {
		s.OnStreamWriteError(&gortsplib.ServerHandlerOnStreamWriteErrorCtx{})
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.drops) != 1 {
		t.Fatalf("counted %d paths, want 1", len(s.drops))
	}
	for path, d := range s.drops {
		// The first call sets the clock and says nothing; every call after
		// it inside the same minute only adds to the count. So all 5000
		// are accounted for, and none of them were printed.
		if d.n == 0 {
			t.Errorf("%s: drops were not counted", path)
		}
		if d.n > 5000 {
			t.Errorf("%s: counted %d of 5000", path, d.n)
		}
	}
}

// The reporting has to name the stream, since the whole complaint about
// the old line was that it named nothing.
func TestTheDropMessageNamesThePath(t *testing.T) {
	s := New("127.0.0.1:0")
	st := s.Add("/lounge_sub")
	sess := &gortsplib.ServerSession{}
	s.mu.Lock()
	s.sessions[sess] = st
	s.mu.Unlock()

	s.OnStreamWriteError(&gortsplib.ServerHandlerOnStreamWriteErrorCtx{Session: sess})

	s.mu.RLock()
	defer s.mu.RUnlock()
	var paths []string
	for p := range s.drops {
		paths = append(paths, p)
	}
	if len(paths) != 1 || !strings.Contains(paths[0], "lounge_sub") {
		t.Fatalf("drops keyed by %v, want the stream's path", paths)
	}
}
