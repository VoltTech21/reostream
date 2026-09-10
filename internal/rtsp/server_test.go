package rtsp

import "testing"

// The two outputs must name the same feed the same way, or an operator
// reading one set of URLs cannot guess the other.
func TestPathMirrorsTheHTTPEndpoints(t *testing.T) {
	for _, tc := range []struct{ cam, stream, want string }{
		{"lounge", "main", "lounge"},
		{"lounge", "sub", "lounge_sub"},
		{"lounge", "extern", "lounge_extern"},
	} {
		if got := Path(tc.cam, tc.stream); got != tc.want {
			t.Errorf("Path(%q, %q) = %q, want %q", tc.cam, tc.stream, got, tc.want)
		}
	}
}
