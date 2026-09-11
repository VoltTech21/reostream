// Accounts: read only, on purpose, with no exception carved out anywhere
// in this file.
//
// Message 59 ("Set user cfg") turned out to be sitting in the same read
// sweep this codebase's own probe uses: `get all` was, for a time, sending
// a user config write with an empty body at live cameras. Nobody has
// established what that write does. baichuan.WriteConfig refuses an empty
// body now, which is the guard that would have stopped it, and this file
// does not add a second, different check that could disagree with that
// one. Instead this file adds no write path at all: no POST, PUT, PATCH or
// DELETE route exists for accounts anywhere in camctl.go, not behind a
// confirmation and not behind a flag. A page that can rewrite a camera's
// accounts is a page that can lock an operator out of their own camera,
// with no way back short of a factory reset, and that risk is not worth
// taking until somebody deliberately establishes what message 59 does.
package camctl

import (
	"context"
	"fmt"
	"net/http"

	"github.com/VoltTech21/reostream/internal/baichuan"
)

// accountsPage is what accounts.html renders: the raw document message 58
// ("get user cfg") answered, and nothing this code has parsed out of it.
//
// This deliberately does not model individual users, permission levels, or
// any other field of the reply: no capture has ever confirmed that shape,
// and inventing one to make a tidier page is exactly the kind of guess this
// codebase's read path refuses everywhere else (see setField's and
// locateLeaf's comments in settings.go). The raw XML is the only thing here
// that is actually known.
type accountsPage struct {
	Title  string
	Camera Camera

	XML string
	Err string
}

// serveAccounts reads message 58 and renders it. There is no corresponding
// POST handler in this file or anywhere else, and none should be added; see
// this file's own top comment.
func (s *Server) serveAccounts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam, err := s.byName(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	page := accountsPage{Title: cam.Name + " accounts", Camera: cam}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	conn, err := s.dial(ctx, cam)
	if err != nil {
		page.Err = fmt.Sprintf("could not connect: %v", err)
		s.render(w, "accounts.html", page)
		return
	}
	defer conn.Close()

	readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
	xml, status, err := baichuan.ReadConfig(readCtx, conn, baichuan.MsgIDGetUserCfg)
	readCancel()
	if err != nil {
		page.Err = fmt.Sprintf("reading accounts: %v", err)
	} else if status != 200 {
		page.Err = fmt.Sprintf("camera answered status %d for the account list", status)
	} else {
		page.XML = string(xml)
	}

	s.render(w, "accounts.html", page)
}
