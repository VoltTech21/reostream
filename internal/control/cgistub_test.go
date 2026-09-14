package control

import (
	"fmt"

	"github.com/VoltTech21/reostream/internal/cgi"
)

// noCGIDial refuses immediately rather than dialling anything. serveCamera
// reads the floodlight over CGI on every visit to a camera's page, so any
// test that hits that page for a reason unrelated to the floodlight still
// needs a CGIDial double: with none, s.opts.CGIDial is nil and cgiDial
// falls back to a real cgi.Dial against the test config's documentation
// range address, which is exactly the real network attempt tests here must
// never make. The resulting FloodlightErr is harmless noise these tests do
// not assert on.
func noCGIDial(Camera) (*cgi.Client, error) {
	return nil, fmt.Errorf("cgi not available in this test")
}
