package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// LogBuffer keeps the most recent log lines in memory and fans new ones out
// to live readers.
//
// It is a tee, not a replacement: the standard logger keeps writing to
// stderr, so docker logs and journald continue to work exactly as they did.
// Nothing here writes a file. Rotation and retention belong to whatever
// supervises the process, and a daemon that manages its own logfile brings
// a class of disk full failures this does not need.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	// partial holds a write that did not end in a newline, so a log line
	// split across two Write calls is not published as two lines.
	partial string
	subs    map[chan string]bool
}

func NewLogBuffer(n int) *LogBuffer {
	return &LogBuffer{max: n, subs: make(map[chan string]bool)}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := b.partial + string(p)
	parts := strings.Split(text, "\n")
	b.partial = parts[len(parts)-1]
	var fresh []string
	for _, line := range parts[:len(parts)-1] {
		b.lines = append(b.lines, line)
		fresh = append(fresh, line)
	}
	if len(b.lines) > b.max {
		b.lines = append([]string(nil), b.lines[len(b.lines)-b.max:]...)
	}

	// Sends happen under the same lock that guards b.subs and that
	// Subscribe's cancel uses to delete and close a channel, so Write can
	// never observe a channel mid-close and send on it. Every send keeps
	// its default case, so it cannot block, which is what makes holding
	// the mutex across a bounded number of these sends safe: it does not
	// reintroduce the stall a slow subscriber must never cause.
	for _, line := range fresh {
		for ch := range b.subs {
			select {
			case ch <- line:
			default:
			}
		}
	}
	return len(p), nil
}

// Lines returns the buffered history, oldest first.
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

// Subscribe returns a channel of new lines and a function that stops it.
func (b *LogBuffer) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 256)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			// delete and close happen under the same lock Write sends
			// under, so Write can never see this channel after it starts
			// closing.
			b.mu.Lock()
			delete(b.subs, ch)
			close(ch)
			b.mu.Unlock()
		})
	}
}

func (s *Server) serveLogsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "logs.html", struct{ Title string }{Title: "Logs"})
}

func (s *Server) serveLogHistory(w http.ResponseWriter, r *http.Request) {
	var lines []string
	if s.opts.Logs != nil {
		lines = s.opts.Logs.Lines()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lines)
}

func (s *Server) serveLogStream(w http.ResponseWriter, r *http.Request) {
	if s.opts.Logs == nil {
		http.Error(w, "no log buffer", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.opts.Logs.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			// http.Server.Shutdown waits for active connections to finish
			// and does not cancel their request contexts, so without this
			// an open logs tab would hold the control server's shutdown for
			// its full timeout. Server.Close closes this channel so a live
			// stream is released as soon as shutdown starts, not five
			// seconds later.
			return
		case line, open := <-ch:
			if !open {
				return
			}
			// A log line containing a newline would end the event early, and
			// Write already split on newlines, so this is only defensive.
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
			flusher.Flush()
		}
	}
}
