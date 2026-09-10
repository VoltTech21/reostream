package control

import (
	"testing"

	"github.com/VoltTech21/reostream/internal/server"
)

func TestStreamStateNamesTheFourCases(t *testing.T) {
	cases := []struct {
		name string
		in   server.StreamStatus
		want string
	}{
		{
			name: "connected and delivering",
			in:   server.StreamStatus{Connected: true, Streaming: true},
			want: "streaming",
		},
		{
			// The held session signature: the connection is up, keepalives
			// are answered, and no picture has arrived. Two streams sat in
			// this state unnoticed for 25 seconds on 2026-09-08, which is
			// why it is a state of its own rather than a slow number.
			name: "connected with no frames",
			in:   server.StreamStatus{Connected: true, Streaming: false},
			want: "novideo",
		},
		{
			name: "down but has run before",
			in:   server.StreamStatus{Connected: false, Restarts: 3},
			want: "reconnecting",
		},
		{
			name: "never came up",
			in:   server.StreamStatus{Connected: false, LastError: "dial tcp: refused"},
			want: "down",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamState(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
