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
	"encoding/json"
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

// rspNotLoggedIn is what a camera returns when it does not recognise the
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

// Dial logs in and returns a session.
//
// The password goes in the request body. Passing it in the query string
// instead fails with "password wrong", which reads like a credential problem
// and is not.
func Dial(host, user, password string) (*Client, error) {
	c := &Client{
		base:     "http://" + host + "/cgi-bin/api.cgi",
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
	if err := c.login(); err != nil {
		return nil, err
	}
	return c, nil
}

// login exchanges the credentials for a fresh token.
func (c *Client) login() error {
	c.token = ""
	var out []reply
	err := c.do("Login", 0, map[string]any{
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

// call runs a command, and logs in again once if the camera says the session
// is not valid. Retrying only that one code keeps a genuine refusal, such as
// a command the model does not implement, reported as itself.
func (c *Client) call(cmd string, action int, param any, out *[]reply) error {
	err := c.do(cmd, action, param, out)
	if err == nil || !isNotLoggedIn(*out) || cmd == "Login" {
		return err
	}
	if err := c.login(); err != nil {
		return err
	}
	return c.do(cmd, action, param, out)
}

// isNotLoggedIn reports whether a reply is the session complaint.
func isNotLoggedIn(out []reply) bool {
	return len(out) > 0 && out[0].Error != nil && out[0].Error.RspCode == rspNotLoggedIn
}

// do sends one command and decodes the reply array.
func (c *Client) do(cmd string, action int, param any, out *[]reply) error {
	body, err := json.Marshal([]command{{Cmd: cmd, Action: action, Param: param}})
	if err != nil {
		return err
	}
	u := c.base + "?cmd=" + url.QueryEscape(cmd)
	if c.token != "" {
		u += "&token=" + url.QueryEscape(c.token)
	}
	resp, err := c.http.Post(u, "application/json", bytes.NewReader(body))
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
		return fmt.Errorf("cgi: %s: %s (rspCode %d)", cmd, e.Detail, e.RspCode)
	}
	return nil
}

// Get runs a read command. action 1 asks the camera to return the valid range
// and the factory default alongside the current value, which is what makes a
// setting adjustable without guessing at bounds.
func (c *Client) Get(cmd string, channel int) (value, initial, rng json.RawMessage, err error) {
	var out []reply
	if err := c.call(cmd, 1, map[string]any{"channel": channel}, &out); err != nil {
		return nil, nil, nil, err
	}
	return out[0].Value, out[0].Initial, out[0].Range, nil
}

// Set runs a write command.
func (c *Client) Set(cmd string, param any) error {
	var out []reply
	return c.call(cmd, 0, param, &out)
}
