package control

import (
	"context"
	"fmt"
	"github.com/VoltTech21/reostream/internal/bcudp"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
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
// license rests on.
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
// costs this whole timeout divided by the concurrency below. 800ms is ample
// for a LAN switch's forwarding delay and a busy camera's accept; a camera
// that needs longer than that to answer a bare TCP connect is not one a
// recorder will pull a steady stream from anyway.
const sweepDialTimeout = 800 * time.Millisecond

// sweepConcurrency is how many dials are in flight at once. With the dial
// timeout above, a /24 of entirely silent addresses finishes in under two
// seconds (ceil(254/96) batches, each at most one dial timeout), rather
// than the ten-plus it took at 32. Still well short of a SYN flood to a
// managed switch -- which on this fleet's own switch is not a hypothetical
// concern, since that switch drops its uplink when it is unhappy -- because
// a connect that completes frees its slot in milliseconds, so the 96 is a
// ceiling on outstanding dials, not a wave of them.
const sweepConcurrency = 96

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
	// UID is set for a battery camera, found by its UDP search reply
	// rather than on port 9000, which it keeps closed. Asleep and Battery
	// are what that reply said.
	UID     string
	Asleep  bool
	Battery int
	// Suggested is a starting point for the name field, not a decision.
	// Derived from the address, so it carries nothing the operator did not
	// already give us -- see suggestedName.
	Suggested string
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
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale", "wg", "tun", "zt", "lxc", "vnet"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func localSweepRanges() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, iface := range ifaces {
		// Container bridges, VPNs and VM networks hold no cameras, and on a
		// Docker host there are many of them.
		if virtualIface(iface.Name) {
			continue
		}
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

// clientSweepRange is the /24 around the address the operator's browser is
// connecting FROM, or "" if that address is not a usable IPv4.
//
// This is the one piece of ground truth the daemon has about the network
// the cameras are on, and it needs no guessing: the person setting up is
// sitting on a machine on that network, and their request carries its
// address. localSweepRanges reads the daemon's own interfaces, which in
// the container this ships as are Docker bridges and never where the
// cameras are -- so for every real install the browser's source address is
// a better default than anything the daemon can see about itself.
//
// Loopback and unspecified are rejected: a request from 127.0.0.1 is
// someone reaching the page through an SSH or docker port-forward, which
// says nothing about where the cameras are, so we fall back to the
// interface list rather than offer to sweep loopback.
//
// Read from RemoteAddr only, never a forwarded header: this value also
// decides whether an unusual range is swept without confirmation (see
// serveDiscover), and a spoofable X-Forwarded-For must not be able to lift
// that confirmation. Behind a reverse proxy RemoteAddr is the proxy, so
// the prefill falls back and the operator confirms as before -- no worse
// than today, and not forgeable.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func clientSweepRange(r *http.Request) string {
	// Behind Home Assistant's ingress RemoteAddr is the Supervisor, on
	// Home Assistant's own internal network, where no camera is.
	if viaIngress(r) {
		return ""
	}
	addr, err := netip.ParseAddr(hostOfAddr(r.RemoteAddr))
	if err != nil || !addr.Is4() {
		return ""
	}
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
		return ""
	}
	// A browser on a VPN such as Tailscale (100.64.0.0/10) is not on the
	// cameras' network, and sweeping its /24 only makes the scan slower.
	if cgnat.Contains(addr) {
		return ""
	}
	return netip.PrefixFrom(addr, 24).Masked().String()
}

// hostSweepRange is the /24 of the address the browser used to reach this
// page, when that is a private IPv4. In a container the interfaces are the
// container's own network, while this address is the host's, where the
// cameras almost always are.
func hostSweepRange(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	ip, err := netip.ParseAddr(h)
	if err != nil || !ip.Is4() || !ip.IsPrivate() {
		return ""
	}
	return netip.PrefixFrom(ip, 24).Masked().String()
}

// sweepRanges is what the setup page offers, best guess first: the range
// the operator's own browser is on, then whatever the daemon's interfaces
// suggest, deduplicated.
func sweepRanges(r *http.Request) []string {
	out := localSweepRanges()
	if client := clientSweepRange(r); client != "" {
		dedup := out[:0:0]
		for _, s := range out {
			if s != client {
				dedup = append(dedup, s)
			}
		}
		out = append([]string{client}, dedup...)
	}
	return out
}

// configuredSubnets is the /24 around each camera already in the config, in
// a stable order, de-duplicated.
//
// It is what lets the setup page find a second camera network on its own:
// once a camera on 10.20.0.x has been added, that subnet is known to hold
// cameras, so the next visit scans it without anyone naming it. Reading the
// config is local and opens no connection; the sweep that follows is the
// only thing that dials.
func configuredSubnets(configPath string) []string {
	var out []string
	seen := make(map[string]bool)
	for host := range configuredHosts(configPath) {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is4() {
			continue
		}
		p := netip.PrefixFrom(addr, 24).Masked().String()
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// autoSweepRanges is what the setup page scans with nothing typed: the
// subnet the operator's browser is on, then the subnet of every camera
// already configured, de-duplicated and in that priority order. The
// browser's own subnet leads because it is where a first-time user, who has
// no configured cameras yet, certainly is.
//
// The daemon's own interface ranges are deliberately NOT included: in the
// container this ships as they are Docker bridges, never where the cameras
// are, so auto-scanning them is a guaranteed-empty sweep that only costs
// time. They remain available for the manual field (sweepRanges), where an
// operator who knows what they are looking at can still pick one.
func autoSweepRanges(r *http.Request, configPath string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(clientSweepRange(r))
	add(hostSweepRange(r.Host))
	for _, s := range configuredSubnets(configPath) {
		add(s)
	}
	// This machine's own networks: where a battery camera on the LAN is
	// when no configured camera shares its subnet.
	for _, s := range localSweepRanges() {
		add(s)
	}
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
//   - /24 or smaller. A /16 is 65,534 dials; at this concurrency that is
//     over half an hour of a machine holding connections open to a network
//     it was not asked to enumerate. The size bound is what keeps a sweep
//     a sweep rather than a campaign, and it is the one limit that remains.
//
// There is deliberately no "is this range yours" check. An earlier version
// refused any range outside RFC1918 unless the operator ticked a
// confirmation, to stop a typo becoming a scan of somebody else's network.
// It was removed on purpose: isolated camera VLANs are routinely built on
// space that is not RFC1918 (this fleet's is 169.x), so the confirmation
// fired on exactly the networks the page exists to serve, every time, and
// read as the page refusing to do its job. The real protections are
// elsewhere and untouched -- the page is behind a password, so whoever can
// submit the form can already read and write every camera, and the /24
// bound keeps any one sweep small -- so what the confirmation added over
// those was friction, not safety.
func parseSweepRange(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	// A bare address means "the /24 around this". The hint on the page
	// already tells an operator that the quickest way to know what to
	// search is the address of a camera they have, so typing that address
	// and having it refused for missing a suffix is the page contradicting
	// its own advice. 192.0.2.11 becomes 192.0.2.0/24.
	if !strings.Contains(s, "/") {
		if addr, err := netip.ParseAddr(s); err == nil && addr.Is4() {
			s = netip.PrefixFrom(addr, 24).Masked().String()
		}
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or a range like 192.168.1.0/24", s)
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s is not IPv4; this sweep only knows how to walk an IPv4 range", s)
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
// DialContext on ctx, so a canceled request aborts the connections
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
// comparing normalized host:port strings would miss a configured camera
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

// discover sweeps one or more ranges and probes everything new it finds.
//
// The probe is s.opts.Probe / probeGuarded, the same path POST
// /setup/probe takes. A second probe implementation would be a second set
// of answers to "what is this camera", and they would drift.
//
// More than one range because the setup page scans, with nothing typed, the
// subnet the operator's browser is on AND the subnet of every camera
// already configured -- a second camera VLAN that already has cameras on it
// is found on the next visit without anyone naming it. The ranges are swept
// in turn (one sweep's worth of dials on the wire at a time, the single
// sweep slot's whole point), and their candidates merged and de-duplicated
// before any probe, so an address that somehow falls in two given ranges is
// identified once.
//
// user defaults to "admin" and pass to empty: a factory camera, or one an
// installer never set a password on, is identified and offered with nothing
// entered. The ones that reject that get a password field of their own on
// the page; this never guesses past the first, blank try.
func (s *Server) discover(ctx context.Context, ranges []netip.Prefix, user, pass string) DiscoverResult {
	if user == "" {
		user = "admin"
	}

	var res DiscoverResult
	labels := make([]string, 0, len(ranges))
	known := configuredHosts(s.opts.ConfigPath)
	seen := make(map[string]bool)
	var candidates []string
	for _, p := range ranges {
		labels = append(labels, p.String())
		res.Scanned += len(hostsIn(p))
		for _, host := range s.sweepRange(ctx, p) {
			if known[host] || seen[host] {
				if known[host] {
					res.Configured++
				}
				continue
			}
			seen[host] = true
			candidates = append(candidates, host)
		}
	}
	res.Range = strings.Join(labels, ", ")
	battery := s.searchBattery(ctx, ranges, seen, known)
	if len(candidates) == 0 {
		res.Found = append(res.Found, battery...)
		return res
	}
	sort.Strings(candidates)

	probe := s.opts.Probe
	if probe == nil {
		probe = s.probeGuarded
	}

	// Concurrent, bounded by the same semaphore size: these are different
	// cameras and therefore different sessions, so there is nothing to
	// serialize for the protocol's sake (applyAll makes the same argument
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
			rep := probe(ctx, host, user, pass)
			res.Found[i] = DiscoveredCamera{
				Address:   host,
				Report:    rep,
				Suggested: nameFor(rep, host),
			}
		}(i, host)
	}
	wg.Wait()
	res.Found = append(res.Found, battery...)
	return res
}

// searchBattery sends the battery-camera search to every host in ranges
// and returns the cameras that answered it but not port 9000: battery
// cameras, which keep every TCP port closed. The search does not wake a
// sleeping camera; its reply says whether it is asleep and its battery.
func (s *Server) searchBattery(ctx context.Context, ranges []netip.Prefix, onTCP, known map[string]bool) []DiscoveredCamera {
	knownUIDs := map[string]bool{}
	if cams, err := loadFleet(s.opts.ConfigPath); err == nil {
		for _, c := range cams {
			if c.UID != "" {
				knownUIDs[c.UID] = true
			}
		}
	}
	var mu sync.Mutex
	var out []DiscoveredCamera
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for _, p := range ranges {
		for _, a := range hostsIn(p) {
			host := a.String()
			if onTCP[host] || known[host] {
				continue
			}
			select {
			case <-ctx.Done():
				wg.Wait()
				return out
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				info, err := bcudp.Search(sctx, host)
				cancel()
				if err != nil || info.UID == "" || knownUIDs[info.UID] {
					return
				}
				mu.Lock()
				out = append(out, DiscoveredCamera{Address: host, UID: info.UID, Asleep: info.Asleep, Battery: info.Battery, Suggested: suggestedName(host)})
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// serveDiscover is the button. It renders a fragment to be swapped into
// the setup page, the way serveProbe does, rather than a whole page.
func (s *Server) serveDiscover(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The page scans with nothing typed: whatever "range" values the form
	// carries, or, when it carries none, the subnets this install already
	// knows about -- the operator's own, plus every configured camera's.
	// Each is parsed and masked; a bad one is reported rather than silently
	// dropped, so a mistyped manual range still says why.
	wanted := r.Form["range"]
	if len(wanted) == 0 {
		wanted = autoSweepRanges(r, s.opts.ConfigPath)
		if len(wanted) == 0 {
			s.renderDiscover(w, r, DiscoverResult{Err: "there is no network to scan automatically yet. type the address of a camera you have, and reostream will look on its network."})
			return
		}
	}
	var ranges []netip.Prefix
	seen := make(map[string]bool)
	for _, want := range wanted {
		p, err := parseSweepRange(want)
		if err != nil {
			s.renderDiscover(w, r, DiscoverResult{Err: err.Error(), Range: want})
			return
		}
		if seen[p.String()] {
			continue
		}
		seen[p.String()] = true
		ranges = append(ranges, p)
	}

	if !s.beginSweep() {
		s.renderDiscover(w, r, DiscoverResult{Err: "a scan is already running; wait for it to finish. " +
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
	// field that could carry them back to the page. Empty is the norm here:
	// discover tries admin with no password, and the page asks only for the
	// cameras that reject it.
	res := s.discover(ctx, ranges, r.FormValue("username"), r.FormValue("password"))
	s.renderDiscover(w, r, res)
}

// serveIdentify re-probes one camera with a password the operator typed
// into that camera's own field, after the credential-free scan could not
// read it. It renders a single replacement card, swapped in place of the
// one that asked for the password.
//
// It is POST /setup/probe's logic aimed at one discovered address: the
// scan already found it on the port, so this does not dial a range, only
// the one host. The password reaches the probe and nothing else, exactly
// as in a sweep.
func (s *Server) serveIdentify(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	addr := strings.TrimSpace(r.FormValue("address"))
	if addr == "" {
		http.Error(w, "no address", http.StatusBadRequest)
		return
	}
	user := r.FormValue("username")
	if user == "" {
		user = "admin"
	}
	probe := s.opts.Probe
	if probe == nil {
		probe = s.probeGuarded
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+2*time.Second)
	defer cancel()
	rep := probe(ctx, addr, user, r.FormValue("password"))
	cam := DiscoveredCamera{
		Address:   addr,
		Report:    rep,
		Suggested: nameFor(rep, addr),
	}
	s.renderCard(w, r, cam)
}

// renderDiscover writes the fragment. Like serveProbe it clones the
// template set and bypasses the layout, because this is swapped into a
// page that is already open.
func (s *Server) renderDiscover(w http.ResponseWriter, r *http.Request, res DiscoverResult) {
	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t.Funcs(requestFuncs(r))
	if _, err := t.ParseFS(templateFS, "templates/discover.html"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "discover", res); err != nil {
		log.Printf("reostream: control: render discover: %v", err)
	}
}

// renderCard writes one camera's card, the unit serveIdentify swaps in
// place. It is the same "found-camera-card" block the discover fragment
// renders in its loop, so a re-probed camera looks exactly like a scanned
// one.
func (s *Server) renderCard(w http.ResponseWriter, r *http.Request, cam DiscoveredCamera) {
	t, err := s.tmpl.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t.Funcs(requestFuncs(r))
	if _, err := t.ParseFS(templateFS, "templates/discover.html"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "found-camera-card", cam); err != nil {
		log.Printf("reostream: control: render card: %v", err)
	}
}

// nameFor is the name a found camera is offered under: its own name, the
// one the Reolink app shows ("Pano Parking" becomes pano_parking), so
// reostream takes the camera's name rather than the other way round; the
// address-based one only when the camera has none or could not be read.
func nameFor(rep CameraReport, addr string) string {
	if n := slugName(rep.DeviceName); n != "" {
		return n
	}
	return suggestedName(addr)
}
