// Package cgi talks to a Reolink camera's HTTP API.
//
// Baichuan is the better transport and carries everything reostream needs for
// streaming, but a handful of camera settings have no Baichuan message id we
// have recovered: the fisheye view modes and the dual lens stitch parameters
// are both reachable only here for now. The message ids exist, since the
// official NVR sets both over Baichuan; finding them needs a capture of an
// NVR talking to a camera, or disassembly of the dispatch code, and neither
// is done. This package is what makes those features usable meanwhile.
package cgi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is a logged in session against one camera.
//
// The credentials are kept because a camera will intermittently forget a
// session it has just issued, and the only way through that is to log in
// again.
type Client struct {
	base     string
	user     string
	password string
	token    string
	http     *http.Client
}

// rspNotLoggedIn is what a camera returns when it does not recognize the
// token. It arrives on connections that logged in seconds earlier, so it
// means "log in again", not "your password is wrong".
const rspNotLoggedIn = -6

type command struct {
	Cmd    string `json:"cmd"`
	Action int    `json:"action"`
	Param  any    `json:"param,omitempty"`
}

type reply struct {
	Cmd     string          `json:"cmd"`
	Code    int             `json:"code"`
	Value   json.RawMessage `json:"value"`
	Initial json.RawMessage `json:"initial"`
	Range   json.RawMessage `json:"range"`
	Error   *struct {
		Detail  string `json:"detail"`
		RspCode int    `json:"rspCode"`
	} `json:"error"`
}

// Dial logs in and returns a session, with no deadline of its own beyond
// the 20 second per-call timeout every Client carries. DialContext is the
// same call with a caller-supplied bound; this is that call with
// context.Background(), for the many call sites that predate ctx threading
// through this package and have no deadline of their own to hand down.
//
// The password goes in the request body. Passing it in the query string
// instead fails with "password wrong", which reads like a credential problem
// and is not.
func Dial(host, user, password string) (*Client, error) {
	return DialContext(context.Background(), host, user, password)
}

// DialContext is Dial with a context that bounds the login call itself, so
// a caller with its own deadline (a fleet apply's per-camera timeout, for
// one) can have it actually interrupt a hung login rather than only bound
// the gap between calls.
//
// It tries HTTP first, then HTTPS. Newer firmware ships with HTTP turned
// off, and HTTPS uses a self-signed certificate, so it is not verified.
// Only a failure to reach the API falls through to HTTPS; a camera that
// answers and refuses the login is reported as it is.
func DialContext(ctx context.Context, host, user, password string) (*Client, error) {
	c := &Client{
		base:     "http://" + host + "/cgi-bin/api.cgi",
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
	httpErr := c.login(ctx)
	if httpErr == nil {
		return c, nil
	}
	var refused *replyError
	if errors.As(httpErr, &refused) || ctx.Err() != nil {
		return nil, httpErr
	}
	c.base = "https://" + host + "/cgi-bin/api.cgi"
	c.http = &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	httpsErr := c.login(ctx)
	if httpsErr == nil {
		return c, nil
	}
	if errors.As(httpsErr, &refused) {
		return nil, httpsErr
	}
	return nil, &unreachableError{fmt.Errorf("cgi: %s: could not reach the camera's HTTP or HTTPS API; "+
		"turn one of them on in the Reolink app under Network > Advanced > Port Settings "+
		"(http: %v; https: %v)", host, httpErr, httpsErr)}
}

// unreachableError is a camera with no web API to talk to: neither HTTP nor
// HTTPS answered at all. An original Lumus serves only its media port, 9000.
type unreachableError struct{ error }

func (e *unreachableError) Unwrap() error { return e.error }

// Unreachable reports whether err is Dial finding no web API on the camera,
// as opposed to one that answered and refused the login.
func Unreachable(err error) bool {
	var u *unreachableError
	return errors.As(err, &u)
}

// replyError is a camera answering with an error, as opposed to a request
// that never got an answer from the API.
type replyError struct {
	cmd     string
	detail  string
	rspCode int
}

func (e *replyError) Error() string {
	return fmt.Sprintf("cgi: %s: %s (rspCode %d)", e.cmd, e.detail, e.rspCode)
}

// login exchanges the credentials for a fresh token.
func (c *Client) login(ctx context.Context) error {
	c.token = ""
	var out []reply
	err := c.do(ctx, "Login", 0, map[string]any{
		"User": map[string]string{"userName": c.user, "password": c.password},
	}, &out)
	if err != nil {
		return err
	}
	var v struct {
		Token struct {
			Name string `json:"name"`
		} `json:"Token"`
	}
	if err := json.Unmarshal(out[0].Value, &v); err != nil {
		return fmt.Errorf("cgi: parse login: %w", err)
	}
	if v.Token.Name == "" {
		return fmt.Errorf("cgi: login returned no token")
	}
	c.token = v.Token.Name
	return nil
}

// Close hands the session back to the camera.
//
// This is not housekeeping, it is the difference between a UI that works
// and one that stops: a Reolink camera allows only a handful of concurrent
// CGI logins, and it holds an abandoned one until its own timeout, which is
// minutes. Every page that reads a setting dials a client, so without this
// a few minutes of ordinary browsing exhausts the camera and every
// subsequent read fails with "max session" -- at which point the camera
// also refuses the Reolink app and its own web UI, not just this one.
//
// Failures are returned but rarely worth acting on: the session is being
// dropped either way, and a Logout that did not arrive is exactly the leak
// that would have happened anyway. Safe on a nil or already-closed client
// so it can sit under an unconditional defer.
func (c *Client) Close() error {
	if c == nil || c.token == "" {
		return nil
	}
	// Its own short bound rather than the caller's context: Close usually
	// runs from a defer while the request that owns that context is being
	// torn down, and a canceled context would skip the logout -- leaking
	// the session this call exists to return.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []reply
	err := c.do(ctx, "Logout", 0, nil, &out)
	c.token = ""
	return err
}

// call runs a command, and logs in again once if the camera says the session
// is not valid. Retrying only that one code keeps a genuine refusal, such as
// a command the model does not implement, reported as itself.
func (c *Client) call(ctx context.Context, cmd string, action int, param any, out *[]reply) error {
	err := c.do(ctx, cmd, action, param, out)
	if err == nil || !isNotLoggedIn(*out) || cmd == "Login" {
		return err
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	return c.do(ctx, cmd, action, param, out)
}

// isNotLoggedIn reports whether a reply is the session complaint.
func isNotLoggedIn(out []reply) bool {
	return len(out) > 0 && out[0].Error != nil && out[0].Error.RspCode == rspNotLoggedIn
}

// do sends one command and decodes the reply array. The request carries
// ctx, so a caller's deadline or cancellation actually aborts this call
// in flight rather than only bounding the gap between calls: the 20 second
// http.Client timeout below is a backstop, not the mechanism a caller's own
// bound relies on.
func (c *Client) do(ctx context.Context, cmd string, action int, param any, out *[]reply) error {
	body, err := json.Marshal([]command{{Cmd: cmd, Action: action, Param: param}})
	if err != nil {
		return err
	}
	u := c.base + "?cmd=" + url.QueryEscape(cmd)
	if c.token != "" {
		u += "&token=" + url.QueryEscape(c.token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cgi: %s: %w", cmd, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cgi: %s: %w", cmd, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cgi: %s: %w", cmd, err)
	}
	// Decoding an array into a slice that already holds replies reuses the
	// elements, and a field absent from the new document keeps its old
	// value. Left alone, the error from a previous reply survives into a
	// successful one and the call reports a failure that did not happen.
	*out = nil
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cgi: %s: parse reply: %w", cmd, err)
	}
	if len(*out) == 0 {
		return fmt.Errorf("cgi: %s: empty reply", cmd)
	}
	if e := (*out)[0].Error; e != nil {
		// A camera reports an unsupported command as an error rather than
		// failing the request, so this is also how a capability is probed.
		return &replyError{cmd: cmd, detail: e.Detail, rspCode: e.RspCode}
	}
	return nil
}

// Get runs a read command. action 1 asks the camera to return the valid range
// and the factory default alongside the current value, which is what makes a
// setting adjustable without guessing at bounds.
//
// ctx bounds this call and, if the session needs a retry, the login that
// follows it: canceling ctx aborts whichever of those is in flight, rather
// than only being honored between round trips the way a caller-side
// context.WithTimeout that this method never looked at would be.
func (c *Client) Get(ctx context.Context, cmd string, channel int) (value, initial, rng json.RawMessage, err error) {
	var out []reply
	if err := c.call(ctx, cmd, 1, map[string]any{"channel": channel}, &out); err != nil {
		return nil, nil, nil, err
	}
	return out[0].Value, out[0].Initial, out[0].Range, nil
}

// Set runs a write command, bounded by ctx the same way Get is.
func (c *Client) Set(ctx context.Context, cmd string, param any) error {
	var out []reply
	return c.call(ctx, cmd, 0, param, &out)
}
