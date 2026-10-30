package control

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// This file lets the page be served under a path prefix rather than at "/".
//
// Two things put it there. Home Assistant's add-on ingress proxies the page
// at /api/hassio_ingress/<token>/ and says so in an X-Ingress-Path header on
// every request, having already stripped that prefix from the path it
// forwards. A reverse proxy an operator runs themselves can do the same
// with [control] base_path, and may or may not strip the prefix before
// forwarding, so both shapes are accepted.
//
// The handlers themselves keep thinking in root paths: every route, every
// redirect target and every "return" field is still "/cameras/...". The
// prefix is added at the edges instead -- by withBase for redirects, by the
// "base" and "at" template functions for what a page links to, by the
// reostream-base meta tag for the page's own scripts, and by cookiePath for
// cookies. With no prefix none of those change a single byte, which is
// what keeps the plain "/" install exactly as it was.

// ingressProxyAddr is the Supervisor's own address on Home Assistant's
// internal network, the only address its ingress proxy connects from.
// X-Ingress-Path is trusted from there and nowhere else: anybody on the LAN
// can send the header, but nobody on the LAN can open a TCP connection from
// this address.
const ingressProxyAddr = "172.30.32.2"

type baseKey struct{}
type ingressKey struct{}

// basePath is the prefix this request's page lives under, "" for the root.
// It never ends in a slash, so basePath(r)+"/cameras" is always right.
func basePath(r *http.Request) string {
	b, _ := r.Context().Value(baseKey{}).(string)
	return b
}

// viaIngress reports whether this request came through Home Assistant's
// ingress proxy on an install that trusts it. Home Assistant has already
// signed the user in by then, so such a request is not asked for
// reostream's own password or claim; see wrap and claimGate.
func viaIngress(r *http.Request) bool {
	v, _ := r.Context().Value(ingressKey{}).(bool)
	return v
}

// cleanBase normalizes a configured or forwarded prefix to "/a/b" form, or
// returns "" when there is none or it is not something this page can be
// trusted to paste into a URL, a Location header and a cookie path. The
// character set is deliberately narrow: an ingress token is hex and
// dashes, and an operator's own prefix has no reason to need more than
// path-safe characters.
func cleanBase(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" || p[0] != '/' || strings.HasPrefix(p, "//") || strings.Contains(p, "/../") ||
		strings.HasSuffix(p, "/..") {
		return ""
	}
	for _, c := range p {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("/-_.~", c):
		default:
			return ""
		}
	}
	return p
}

// fromIngressProxy reports whether the connection itself, not any header
// on it, comes from the Supervisor.
func fromIngressProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == ingressProxyAddr
}

// withBase works out this request's prefix, records it on the request, and
// puts the prefix back on any redirect a handler writes. It wraps the whole
// route table, outside claimGate, because the claim gate's own redirect has
// to keep the prefix too.
func (s *Server) withBase(next http.Handler) http.Handler {
	static := cleanBase(s.opts.BasePath)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := static
		ingress := false
		if s.opts.HAIngress && fromIngressProxy(r) {
			if p := cleanBase(r.Header.Get("X-Ingress-Path")); p != "" {
				// Ingress strips its prefix before forwarding, so the path
				// is already a root path and only the links need it back.
				base, ingress = p, true
			}
		}
		if !ingress && base != "" {
			// An operator's own proxy may forward the prefix or strip it.
			// Strip it here when it is present, so either works.
			if r.URL.Path == base {
				r.URL.Path = "/"
			} else if rest, ok := strings.CutPrefix(r.URL.Path, base+"/"); ok {
				r.URL.Path = "/" + rest
			}
			r.URL.RawPath = ""
		}
		if base == "" && !ingress {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), baseKey{}, base)
		ctx = context.WithValue(ctx, ingressKey{}, ingress)
		r = r.WithContext(ctx)
		if ingress && !ingressOriginOK(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(&baseWriter{ResponseWriter: w, base: base}, r)
	})
}

// ingressOriginOK refuses a state-changing request that a browser says came
// from some other site. Behind ingress there is no reostream session cookie
// whose SameSite attribute would do this, so it is checked here instead,
// the same way sameOriginPost does for the claim. The browser's host may
// arrive as Host or only as X-Forwarded-Host depending on how the request
// was relayed, so either matching is enough; both are set by Home
// Assistant, not by the browser, on a connection already known to come
// from the Supervisor.
func ingressOriginOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// The talk WebSocket is a GET, and its upgrader makes the same
		// check itself through originMatches.
		return true
	}
	return originMatches(r)
}

// originMatches reports whether the request's Origin, when it has one,
// names the host the page was reached on.
func originMatches(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if viaIngress(r) {
		for _, h := range strings.Split(r.Header.Get("X-Forwarded-Host"), ",") {
			if strings.EqualFold(u.Host, strings.TrimSpace(h)) {
				return true
			}
		}
	}
	return false
}

// pageHost is the host the browser used to reach this page, which is what
// a recorder URL shown on it should name. Behind ingress the request may
// arrive addressed to the add-on's internal address, so the host Home
// Assistant was reached on is taken from X-Forwarded-Host instead -- only
// there, where the header is known to come from the Supervisor.
func pageHost(r *http.Request) string {
	if viaIngress(r) {
		if h, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(h) != "" {
			return hostOnly(strings.TrimSpace(h))
		}
	}
	return hostOnly(r.Host)
}

// at puts the request's prefix on a root path, and leaves anything else --
// an absolute URL such as a -stream-base tile, a relative path, "" --
// exactly as it is. It is what a template uses for a URL that came from Go
// rather than being written out in the template.
func at(base, u string) string {
	if base == "" || !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
		return u
	}
	if u == base || strings.HasPrefix(u, base+"/") {
		return u
	}
	return base + u
}

// cookiePath scopes a cookie to the page's own prefix, so that under
// ingress it is not sent to the rest of Home Assistant.
func cookiePath(r *http.Request) string {
	return basePath(r) + "/"
}

// baseWriter puts the prefix on a root-path Location header as the
// response's header is written. Redirects are written in some seventy
// places as root paths, and doing it here rather than at each one is what
// stops the next one written from forgetting.
type baseWriter struct {
	http.ResponseWriter
	base  string
	wrote bool
}

func (w *baseWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if loc := w.Header().Get("Location"); loc != "" {
			w.Header().Set("Location", at(w.base, loc))
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *baseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush, Hijack and Unwrap keep what the wrapped writer could do: the log
// page streams, and talk upgrades to a WebSocket.
func (w *baseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *baseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("control: connection cannot be hijacked")
}

func (w *baseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
