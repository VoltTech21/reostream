# One page, one install

Fold the camera control page into the operator page, and make a fresh install
work with nothing but a `docker run` line: no config file written by hand, no
password chosen in advance, no knowledge of TOML.

This supersedes the two-process split described in
`docs/design/2026-09-11-camera-control-ui.md`. That document's reasoning about
what the camera control surface DOES still stands; only its placement changes.

## Why the split is being undone

The split was justified on the grounds that a fault in camera control must never
be able to take video down. That argument was weaker than it looked. Go's HTTP
server already recovers a panic in a handler: it kills the request, not the
process. The isolation being bought was mostly imaginary. What can genuinely
harm streaming is a resource leak, such as leaked camera connections exhausting
file descriptors, and that is a thing to fix rather than a thing to partition
around.

The split's real cost is measurable. Three bugs found during its first live run
existed only because there were two programs:

- One session cookie name shared by two processes on one host. Cookies are not
  port scoped, so logging into one page silently logged the operator out of the
  other.
- `config.Load` resolving the `[control]` password, which lives in the streaming
  daemon's environment and not the control tool's, so every page returned 500 on
  a normal deployment.
- Two layouts and two stylesheets to keep in step.

For an install meant to be usable by someone who does not code, two URLs, two
passwords, two processes and two things to deploy is the wrong shape.

## What stays separate

`cmd/reocam` keeps its command line. `probe`, `get`, `set`, `snap` and `talk`
are how protocol work actually gets done, and the probe is the only honest
answer to what a model implements. Only `reocam serve` goes away.

## Shape

One binary serving three ports rather than four:

| port | what |
|---|---|
| 8560 | streams, unauthenticated, what a recorder points at |
| 8554 | RTSP, when `[rtsp]` is configured. No default: the operator sets `[rtsp].listen`, and 8554 is the conventional port the README and `internal/rtsp` use in every example |
| 8562 | the page, behind a password |

`internal/camctl` folds into `internal/control` as a section rather than a
package with its own server, its own auth and its own layout.

## One camera, not two

The most useful part of this change is not the process count.

Today two different things are called a camera. reostream has config entries: a
name, an address, and which streams to pull. The control page has physical
devices whose settings can be read and written. A person has one camera in their
head, not two.

Combined, a camera has two aspects: how it is streamed, and how it is
configured. One page per camera carries its stream state and video at the top,
its curated settings below, and its raw blocks behind an advanced link.

Navigation: Status, Cameras, Logs, Config, Setup.

## First run

    docker run -v reostream-data:/data -p 8560:8560 -p 8562:8562 <image>

No config file. No environment variable. No password chosen in advance. The
daemon starts, streams nothing, and serves a claim screen. Setting a password
and adding a camera writes `/data/config.toml`.

Four properties of that first run matter more than the convenience:

**The claim screen only answers a one-time token printed to the log.** The
hazard is unchanged and must stay explicit: claim-on-first-visit means the first
person to arrive owns an install that can write to cameras -- credentials,
accounts, time, every curated setting. If the control port is reachable by a
stranger with a scanner, the install is theirs before the operator ever opens
the page.

What the token asks instead is "can you read this daemon's logs", because that
is the question with an answer worth trusting: reading the logs means
controlling the deployment, which is what owning an install should mean. It is
generated at startup, held in memory only, never written to disk, printed in the
first-run message and repeated with it -- straight to stderr, not through the
log writer that tees into the buffer the Logs page serves -- and spent by the
claim it authorises. A
restart before the install is claimed prints a new one and says so. Sixteen
characters from a 31-character alphabet with no visually ambiguous glyphs (no
0/O, no 1/I/L) -- 79 bits -- compared in constant time, and accepted however it
is typed: any case, with the dashes or without.

This replaces an earlier rule that only answered a private source address. That
rule failed in both directions, from one wrong premise -- that a source address
tells you who may own an install:

- It refused legitimate operators. `netip`'s `IsPrivate` is false for
  100.64.0.0/10, the CGNAT range, which is where every Tailscale address lives.
  Reaching a fresh install over a tailnet, the normal way this fleet is reached,
  was refused on the operator's own daemon.
- It admitted strangers. `docker-proxy` is a plain TCP relay that adds no HTTP
  headers, so a published container port makes every request arrive from
  127.0.0.1 or 172.17.0.1 with nothing to notice. The same goes for Docker
  Desktop's gateway, a Kubernetes NodePort with `externalTrafficPolicy: Cluster`,
  nginx or HAProxy in stream mode, socat and `ssh -L`. This repo ships a
  Dockerfile and a compose file, so that is the product's own deployment shape.

No longer header list and no trusted-proxy knob fixes a wrong premise. A token
works identically over a tailnet, behind a reverse proxy and behind an L4 hop,
and is what Jupyter, Portainer and Home Assistant all do.

**There is no rate limit on `POST /login`, and that is a decision.** A reader
who goes looking for a throttle here will not find one, so the reasoning is
written down rather than left to be inferred as an oversight.

A rate limit has to be keyed by something, and the only thing on offer is the
request's source. That is exactly what this page cannot trust: `docker-proxy` is
a plain TCP relay, so behind the Dockerfile and compose file this repo ships,
every request arrives from 127.0.0.1 or 172.17.0.1. The attacker's key IS the
operator's. Four designs were built and measured against that before it was
accepted:

| round | mechanism | what it did |
|---|---|---|
| 1 | 5 failures then a 5 minute lockout | any passer-by refused the OPERATOR's correct password |
| 2 | per-request delay, 64-sleeper cap, 503 past it | the correct password was refused at 32 req/s |
| 3 | per-key sleeper limit, overflow checked at once | circular -- the attacker creates the overflow: **8,399 checks/sec** at 8 workers |
| 4 | per-key leaky bucket, 4096 global guard | **8,355 checks/sec** at 4,200 connections, and 5s of ABANDONED requests pushed the operator's own queue 11h27m out |

That is one fact rather than four bugs. Rate limiting works by refusing or
delaying. When the attacker is indistinguishable from the victim, both of those
are weapons handed to the attacker, so a source-keyed limit can refuse the
operator, delay the operator, or bound nothing. There is no fourth outcome, and
no fifth round would have found one. Rounds 3 and 4 are the instructive pair:
both were built specifically so that nothing could ever be refused, and both
therefore had an escape the attacker could open at will.

`serveLogin` now compares the password and answers, with nothing in front of it.
The security moves to the one quantity an attacker cannot touch: the entropy of
the password.

**The claim screen generates a password, and offers it first.** Every render of
the form calls `crypto/rand` and puts a fresh 16-character password from the
token's 31-character unambiguous alphabet -- 79.3 bits -- into the field as
readable text, with wording that says it can be used as it is or replaced.

Printing a secret in a response body deserves a second look, and it survives
one. It is not a credential until somebody submits it: nothing stores it,
nothing logs it, nothing remembers it between requests, and the server does not
know which of the strings it has handed out -- if any -- will come back. An
attacker who GETs `/claim` a thousand times collects a thousand unrelated random
strings that tell them nothing about what the operator eventually types. What
would let them claim the install is the token, which reaches only the daemon's
own stderr, and the route 404s outright once the install has an owner. The
containment that keeps this true is narrow on purpose: the field is set only on
renders that draw the form, `claimPage` is only ever passed to `claim.html`, and
it never reaches a log line, an error, a URL or the config-page template.

**The typed minimum is 16 characters**, up from 12. Twelve was derived against a
throttled rate that no longer exists, so the sum is redone against the fastest
rate actually measured with nothing in the way: **~8,400 comparisons a second**
sustained from one source, bounded only by the HTTP server. That is 7.3e8
guesses a day and 2.65e11 a year, so surviving a year takes about 2^39.

- The **generated** password is 2^79.3. Half that space at 8,400/s is 4.6e11
  years. Safe by a margin no rate limit could have bought, which is the point of
  offering it first.
- **Three words** a person will remember is about 2^33 from an everyday
  vocabulary, which falls in about **six days** at that rate. That is why this
  screen no longer recommends three words, and why the floor moved.
- **Four words** is about 2^44, roughly 33 years. Sixteen characters is about
  what four words costs to type, which is where the floor comes from.

What a length rule buys is worth stating honestly: sixteen characters a person
invents is not 79 bits, and nothing here measures entropy. The floor pushes a
hand-chosen password past 2^39 in the typical case; the generated default is the
only thing that guarantees it. That is why the field arrives already filled in
rather than empty with advice next to it.

And it is a default, not an invariant. It applies at claim time only. A password
written by hand into `config.toml`, or changed later through the Config page, is
never checked against it -- an operator who wants a four-character password on
their own LAN can still have one, they just cannot get one by accident on the
first screen.

Length only: no complexity classes, no strength meter, no dictionary of common
passwords. The rule is stated plainly on the screen and in the refusal, and it is
counted in characters rather than bytes so a password in any script faces the
same rule. This is the first screen a person who does not code will ever see, and
a rule they satisfy by leaving the box alone is worth more than one they satisfy
by adding "1!" to something short.

The cross-site check on `POST /claim` stays. CSRF is a separate concern from the
claim gate: the token blunts a forged claim but does not close it, since an
operator with the claim screen open has just read the token out of their own
logs.

**Unclaimed is loud.** The log says, repeatedly, that there is no config, that
the setup page is being served, and that nothing has claimed it yet. A half
finished install should be visible rather than sitting quietly.

**Zero cameras is a valid state, not an error.** The supervisor runs with an
empty fleet, and the status page says so plainly and points at setup. Today an
empty camera list fails validation, which is correct for a daemon that is
configured by hand and exactly wrong for one that configures itself.

## Where the config lives

A data directory the container owns, with the config inside it. One mount,
writable, surviving upgrades, and backed up by copying a directory.

This replaces the single file bind mount in the compose file, which is wrong
twice over: the file has to exist before the container starts, which is the
thing being removed, and a single file bind mount is what defeated atomic
replacement during the operator page's live verification. The mount is also
currently read only, which contradicts a page that edits the config.

`-config` remains as an explicit override, so an existing deployment keeps
working unchanged and only new installs get the `/data` convention.

## What this deletes

A merge that only adds is usually the wrong merge. This one removes:

- The second cookie name, and the cross-login bug it caused.
- `config.LoadForDialing`. It exists only because one program needed camera
  passwords without the other's control password. One process needs both, so the
  distinction and the bug that produced it both disappear.
- A second layout, stylesheet, login, password, port and process.
- `reocam serve` and its flags.

## Testing

`internal/fakecam` as everywhere else in this project. No test connects to a
real camera or scans a network.

The claim flow needs its own tests, because it is the one path a new user cannot
avoid: an unclaimed install serves the claim screen; a claimed one does not; a
wrong token is refused and the right one claims; the token is single use and a
restart while unclaimed yields a different one; the compare is constant time
(asserted as a call, not as a timing measurement, and with a mutation that
cannot coincide with the original); zero cameras is valid; and the first save
actually creates the file.

The login has no rate limit, and that end state is what gets tested rather than
the four throttles that were measured and rejected on the way to it. There is no
delay: a wrong password is answered immediately and a correct one is accepted
immediately, whatever else is in flight, and no attempt is ever refused or
deferred because of a different one. The password is what carries the weight, so
the tests are about the password: the claim screen offers a generated one on
every render, freshly generated each time and never stored, logged or reused;
a hand-typed password shorter than the 16-character floor is refused at claim
time with the rule stated; and 16 or more is accepted.

Every state a config file can be in is tested for which side it fails to, since
that is what decides whether the page is open: a password is adopted live
without a restart; `allow_no_password` is honoured; an unparseable file, a
`$NAME` whose variable is unset, and a file with no `[control]` section at all
all leave the install UNCLAIMED with the gate shut, and a gated route on such an
install must redirect rather than serve a camera password into a response body.

Then a live pass, including a first run from genuinely nothing. Every live pass
run against this project so far has found something no unit test could, and the
bootstrap path is the least testable part of the whole system.

## Not in this document

- Publishing a container image, and a quickstart written for someone who does
  not already understand this protocol. Both are needed for the stated goal and
  neither has a design question in it; they are a checklist, not a spec.
- Camera discovery. Adding a camera still means typing its address.
- Battery cameras, which remain untested and need a device.
- PTZ, which remains unimplemented. The ids are known and confirmed against NVR
  firmware; nothing here can move.
