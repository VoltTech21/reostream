package rtsp

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
)

// Server serves camera streams over RTSP.
//
// Paths mirror the HTTP endpoints, so a camera reachable at
// http://host:8560/lounge_sub.ts is also at rtsp://host:8554/lounge_sub.
type Server struct {
	srv *gortsplib.Server

	mu       sync.RWMutex
	streams  map[string]*Stream
	sessions map[*gortsplib.ServerSession]*Stream
}

// New returns a server bound to listen. UDP is offered alongside TCP but is
// not preferred: a lost RTP packet is a corrupt frame with no retransmit,
// which is worse than the larger header interleaving over TCP costs. Clients
// that support both negotiate TCP first.
func New(listen string) *Server {
	s := &Server{
		streams:  make(map[string]*Stream),
		sessions: make(map[*gortsplib.ServerSession]*Stream),
	}
	s.srv = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    listen,
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
		// The per-session write ring is drop-on-full, not block-on-full:
		// when it overflows, gortsplib silently discards the packet. A single
		// fisheye keyframe is a ~1 MB single-slice IDR, which FU-A fragments
		// into ~708 RTP packets pushed in one tight burst. The default queue
		// of 256 overflows mid-keyframe whenever the TCP writer lags the
		// burst, punching holes in the slice that decode as "error while
		// decoding MB 0" on the client. go2rtc hides this by draining fast
		// enough that the ring never fills; a direct player over the network
		// does not. 2048 absorbs a full keyframe with headroom for the
		// P-frames queued behind it. Must be a power of two.
		WriteQueueSize: 2048,
	}
	return s
}

// Add registers a path and returns the stream that feeds it. The returned
// Stream implements stream.FrameSink. Call before Start.
func (s *Server) Add(path string) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &Stream{srv: s.srv}
	s.streams[path] = st
	return st
}

// Start begins listening.
func (s *Server) Start() error {
	if err := s.srv.Start(); err != nil {
		return fmt.Errorf("rtsp: listen on %s: %w", s.srv.RTSPAddress, err)
	}
	return nil
}

// Addr reports the address actually bound, which matters when the configured
// port was 0.
func (s *Server) Addr() string {
	return s.srv.RTSPAddress
}

// Close stops the server and releases every stream.
func (s *Server) Close() {
	s.srv.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.streams {
		st.close()
	}
}

// lookup finds a stream that is ready to be described. A stream that exists
// but has not yet seen a keyframe is deliberately not found: describing it
// would mean an SDP with no parameter sets, which a client caches and then
// fails to decode with.
func (s *Server) lookup(path string) (*Stream, bool) {
	// gortsplib hands the path with a leading slash, and a client may add a
	// trailing one. Neither is part of the name a stream was registered under.
	path = strings.Trim(path, "/")

	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.streams[path]
	if !ok || !st.Ready() {
		return nil, false
	}
	return st, true
}

func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, ok := s.lookup(ctx.Path)
	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st.serverStream(), nil
}

func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, ok := s.lookup(ctx.Path)
	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.Lock()
	s.sessions[ctx.Session] = st
	s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, st.serverStream(), nil
}

// OnPlay is where a reader starts counting. Packetisation is gated on the
// count, so a session that has set up but not played still costs nothing.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.mu.RLock()
	st := s.sessions[ctx.Session]
	s.mu.RUnlock()
	if st != nil {
		st.addReader()
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnSessionClose has to drop the reader whatever ended the session, which
// includes a client that vanished without TEARDOWN. Leaving the count high
// would keep packetising into a void for the life of the process.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	st := s.sessions[ctx.Session]
	delete(s.sessions, ctx.Session)
	s.mu.Unlock()
	if st != nil {
		st.removeReader()
	}
}

// Path returns the RTSP path for one camera stream.
//
// It mirrors the HTTP endpoints so the two outputs name the same feed the
// same way: main is the bare camera name, and the other two take the suffix
// they have on the HTTP side. A camera at http://host:8560/lounge_sub.ts is
// at rtsp://host:8554/lounge_sub.
func Path(camera, stream string) string {
	switch stream {
	case "main":
		return camera
	default:
		return camera + "_" + stream
	}
}
