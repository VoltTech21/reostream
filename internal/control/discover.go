package control

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/VoltTech21/reostream/internal/config"
)

// Finding cameras without being told where they are
//
// Adding a camera means knowing its address. Reolink's own client learns
// that over a UDP discovery protocol this project has never captured:
// docs/protocol.md does not describe it and the message id table recovered
// from firmware contains nothing that looks like it. Guessing at it, or
// reading it out of another implementation, is not available here -- the
// protocol code in this repository is clean-room, and that is what the
// licence rests on.
//
// So this does the boring thing instead: open a TCP connection to every
// address in one /24 and see which ones answer on the Baichuan port. That
// is not a protocol, it is a port sweep, and it is honest about being one.
// It is never automatic: it touches every host on the operator's network,
// including hosts that are not cameras, so a person has to press the
// button.

// baichuanPortDefault is the port every Reolink camera serves Baichuan on,
// and the port config.NormalizeAddr fills in when an address carries none.
const baichuanPortDefault = 9000

// sweepDialTimeout bounds one address's dial. A camera on the same LAN
// answers or refuses in milliseconds; this only has to be long enough to
// cover a switch's forwarding delay and a camera that is busy accepting.
// Longer would be worse, not better: an address with nothing at all on it
// does not refuse, it goes silent, and every silent address in the range
// costs this whole timeout divided by the concurrency below.
const sweepDialTimeout = 1500 * time.Millisecond

// sweepConcurrency is how many dials are in flight at once. Enough that a
// /24 of silent addresses finishes in about ten seconds rather than six
// minutes, and few enough that this does not look like a SYN flood to a
// managed switch -- which on this fleet's own switch is not a hypothetical
// concern, since that switch drops its uplink when it is unhappy.
const sweepConcurrency = 32

// sweepTimeout bounds the whole thing: the dial sweep, and then one setup
// probe per candidate. probeTimeout already bounds each probe, and the
// probes run concurrently, so this is the ceiling on the request as a
// whole rather than a sum of parts.
const sweepTimeout = 3 * time.Minute

// DiscoveredCamera is one address that answered on the Baichuan port,
// with whatever the existing setup probe then made of it.
//
// There is no password field here, and there must never be one. The
// credentials a sweep probes with arrive in the request, live in a local
// variable, and go no further: not into this struct, not into the
// template that renders it, not into a log line, and not into the URL the
// browser is left sitting on.
type DiscoveredCamera struct {
	Address string
	Report  CameraReport
}

// DiscoverResult is one sweep's report. Configured counts addresses that
// answered but are already in the config: they are deliberately not probed
// and not offered, since probing a configured camera is exactly what
// probeGuarded exists to refuse, but an operator who swept and found
// "nothing" deserves to know the reason was that everything found is
// already added.
type DiscoverResult struct {
	Range      string
	Scanned    int
	Found      []DiscoveredCamera
	Configured int
	Err        string
}

// localSweepRanges is the /24 around each non-loopback private IPv4 this
// daemon is bound to, in a stable order, deduplicated.
//
// Reading the interface list is the only way to guess the operator's
// network without asking them, and it is a local read: it opens no
// connection to anything. Loopback is excluded here because a real install
// has no cameras on it; the validator below still accepts loopback,
// because the tests for this sweep on a real network.
func localSweepRanges() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP.To4())
			if !ok || !ip.Is4() {
				continue
			}
			if !ip.IsPrivate() {
				continue
			}
			p := netip.PrefixFrom(ip, 24).Masked()
			if seen[p.String()] {
				continue
			}
			seen[p.String()] = true
			out = append(out, p.String())
		}
	}
	sort.Strings(out)
	return out
}

// parseSweepRange turns an operator-supplied range into a prefix, or
// refuses it.
//
// Two refusals, and both matter for the same reason: this handler opens a
// TCP connection to every address in whatever it is given, so the range is
// an instruction to make outbound connections that somebody typed into a
// form.
//
//   - Private or loopback only. Without this, a form on a daemon that is
//     reachable from anywhere becomes a port scanner pointed at whatever
//     public network the submitter names, run from the operator's address
//     and with the operator's blame attached. RFC1918 and loopback are the
//     only ranges where "every host in here is mine" is a safe assumption.
//   - /24 or smaller. A /16 is 65,534 dials; at this concurrency that is
//     over half an hour of a machine holding connections open to a network
//     it was not asked to enumerate. The size bound is what keeps a sweep
//     a sweep rather than a campaign.
func parseSweepRange(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not a network range like 192.168.1.0/24", s)
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s is not IPv4; this sweep only knows how to walk an IPv4 range", s)
	}
	if !p.Addr().IsPrivate() && !p.Addr().IsLoopback() {
		return netip.Prefix{}, fmt.Errorf(
			"%s is not a private network. only a private range (10.x, 172.16-31.x, 192.168.x) "+
				"or loopback may be swept: this opens a connection to every address in the range, "+
				"and pointing that at the public internet would be a port scan of somebody else's network", s)
	}
	if p.Bits() < 24 {
		return netip.Prefix{}, fmt.Errorf(
			"%s is larger than a /24. a bigger range is thousands of connections, so it is refused rather than started", s)
	}
	return p, nil
}

// hostsIn lists the addresses in p that could be a camera: every address
// in the range, less the network and broadcast addresses, which nothing
// answers on.
func hostsIn(p netip.Prefix) []netip.Addr {
	p = p.Masked()
	var out []netip.Addr
	for addr := p.Addr(); p.Contains(addr); addr = addr.Next() {
		if !addr.IsValid() {
			break
		}
		out = append(out, addr)
	}
	// /31 and /32 have no network or broadcast address to drop; anything
	// wider does, and dialling either is a guaranteed wasted timeout.
	if p.Bits() <= 30 && len(out) >= 2 {
		out = out[1 : len(out)-1]
	}
	return out
}

// baichuanPort is the port the sweep dials. Tests override it so they can
// sweep loopback against a listener they bound themselves; nothing else
// ever sets it, and a real install always dials 9000.
func (s *Server) baichuanPort() int {
	if s.sweepPort != 0 {
		return s.sweepPort
	}
	return baichuanPortDefault
}

// sweepRange dials every host in p and returns the ones that accepted, in
// address order.
//
// Bounded: at most sweepConcurrency dials are in flight, held by a
// buffered channel used as a semaphore. Cancellable: every dial is a
// DialContext on ctx, so a cancelled request aborts the connections
// already in progress rather than only stopping new ones from starting,
// and the semaphore acquire selects on ctx.Done too so the queued
// addresses stop being dialled at all. Leak-free: this does not return
// until wg.Wait does, so when the handler's request context ends there is
// no goroutine still dialling on behalf of a page nobody has open.
func (s *Server) sweepRange(ctx context.Context, p netip.Prefix) []string {
	hosts := hostsIn(p)
	port := s.baichuanPort()

	var (
		mu   sync.Mutex
		live []string
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, sweepConcurrency)
	dialer := &net.Dialer{Timeout: sweepDialTimeout}

	for _, host := range hosts {
		select {
		case <-ctx.Done():
			// Stop handing out work. The goroutines already running are
			// waited for below, so this returns what was learned rather
			// than abandoning anything mid-dial.
			wg.Wait()
			sort.Strings(live)
			return live
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(host netip.Addr) {
			defer wg.Done()
			defer func() { <-sem }()

			addr := net.JoinHostPort(host.String(), fmt.Sprint(port))
			dialCtx, cancel := context.WithTimeout(ctx, sweepDialTimeout)
			defer cancel()
			conn, err := dialer.DialContext(dialCtx, "tcp", addr)
			if err != nil {
				return
			}
			// Closed immediately, without writing a byte. A camera allows
			// one session per stream and holds a session open after the
			// peer goes away; a sweep that left 254 half-open connections
			// behind would be the very thing this daemon spends most of
			// its care avoiding.
			conn.Close()

			mu.Lock()
			live = append(live, host.String())
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	sort.Strings(live)
	return live
}

// beginSweep claims the one sweep slot, and reports whether the claim
// succeeded. This is inFlightProbes' argument at fleet scale: two browser
// tabs each starting a sweep would put 64 dials on the wire and then probe
// the same newly-found cameras twice concurrently, which is the
// one-session-per-stream hazard probeGuarded exists to prevent, arrived at
// from a different direction.
//
// One slot rather than one per range, deliberately: unlike a probe, whose
// hazard is scoped to the camera it dials, the hazard here is the load the
// sweep puts on the network, and two sweeps of two different ranges put
// twice as much of it on the same wire.
func (s *Server) beginSweep() bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	if s.sweeping {
		return false
	}
	s.sweeping = true
	return true
}

// endSweep releases the claim made by beginSweep.
func (s *Server) endSweep() {
	s.inFlightMu.Lock()
	s.sweeping = false
	s.inFlightMu.Unlock()
}

// configuredHosts is the set of host addresses already in the config, with
// any port stripped.
//
// Host only, not host:port: the sweep only ever finds hosts listening on
// the Baichuan port, so the port carries no information here, and
// comparing normalised host:port strings would miss a configured camera
// written with an explicit non-default port.
func configuredHosts(configPath string) map[string]bool {
	out := make(map[string]bool)
	if configPath == "" {
		return out
	}
	// LoadRaw, not Load: this only needs addresses, and Load would demand
	// every "$NAME" password reference resolve from the environment just
	// to answer which hosts are already known.
	cfg, err := config.LoadRaw(configPath)
	if err != nil {
		return out
	}
	for _, cam := range cfg.Cameras {
		host := cam.Address
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		out[host] = true
	}
	return out
}

// discover runs one whole sweep: dial the range, drop what is already
// configured, and ask the rest what they are through the existing setup
// probe.
//
// The probe is s.opts.Probe / probeGuarded, the same path POST
// /setup/probe takes. A second probe implementation would be a second set
// of answers to "what is this camera", and they would drift.
func (s *Server) discover(ctx context.Context, p netip.Prefix, user, pass string) DiscoverResult {
	res := DiscoverResult{Range: p.String()}

	live := s.sweepRange(ctx, p)
	res.Scanned = len(hostsIn(p))

	known := configuredHosts(s.opts.ConfigPath)
	var candidates []string
	for _, host := range live {
		if known[host] {
			res.Configured++
			continue
		}
		candidates = append(candidates, host)
	}
	if len(candidates) == 0 {
		return res
	}

	probe := s.opts.Probe
	if probe == nil {
		probe = s.probeGuarded
	}

	// Concurrent, bounded by the same semaphore size: these are different
	// cameras and therefore different sessions, so there is nothing to
	// serialise for the protocol's sake (applyAll makes the same argument
	// for a fleet write). Serially, a range holding four cameras that are
	// each holding a dead session would cost four probeTimeouts in a row.
	res.Found = make([]DiscoveredCamera, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, sweepConcurrency)
	for i, host := range candidates {
		select {
		case <-ctx.Done():
			wg.Wait()
			res.Found = res.Found[:i]
			return res
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			defer func() { <-sem }()
			// Indexed write into a pre-sized slice, so the report keeps
			// address order rather than whichever camera answered first.
			res.Found[i] = DiscoveredCamera{Address: host, Report: probe(ctx, host, user, pass)}
		}(i, host)
	}
	wg.Wait()
	return res
}

// serveDiscover is the button. It renders a fragment to be swapped into
// the setup page, the way serveProbe does, rather than a whole page.
func (s *Server) serveDiscover(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The range defaults to whatever this daemon's own interfaces suggest,
	// so the ordinary case needs nothing typed; an operator on a network
	// the daemon is not itself bound to can name one, and parseSweepRange
	// is what keeps that from becoming a scanner for hire.
	want := r.FormValue("range")
	if want == "" {
		ranges := localSweepRanges()
		if len(ranges) == 0 {
			s.renderDiscover(w, DiscoverResult{Err: "this machine has no private IPv4 address, so there is no network to sweep. name a range instead."})
			return
		}
		want = ranges[0]
	}
	p, err := parseSweepRange(want)
	if err != nil {
		s.renderDiscover(w, DiscoverResult{Err: err.Error()})
		return
	}

	if !s.beginSweep() {
		s.renderDiscover(w, DiscoverResult{Range: p.String(), Err: "a sweep is already running; wait for it to finish. " +
			"two at once would put twice the dials on the same wire and probe the same new cameras twice."})
		return
	}
	defer s.endSweep()

	// r.Context(), so closing the page stops the sweep: a person who
	// clicks and navigates away must not leave 254 dials in flight.
	ctx, cancel := context.WithTimeout(r.Context(), sweepTimeout)
	defer cancel()

	// The credentials go straight into the probe and nowhere else. They
	// are not stored, not logged, and not rendered: DiscoverResult has no
	// field that could carry them back to the page.
	res := s.discover(ctx, p, r.FormValue("username"), r.FormValue("password"))
	s.renderDiscover(w, res)
}

// renderDiscover writes the fragment. Like serveProbe it clones the
// template set and bypasses the layout, because this is swapped into a
// page that is already open.
func (s *Server) renderDiscover(w http.ResponseWriter, res DiscoverResult) {
	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := t.ParseFS(templateFS, "templates/discover.html"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "discover", res); err != nil {
		log.Printf("reostream: control: render discover: %v", err)
	}
}
