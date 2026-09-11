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
| 8561 | RTSP, when `[rtsp]` is configured |
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

Three properties of that first run matter more than the convenience:

**The claim screen only answers a private source address.** An unclaimed install
whose port is exposed to the internet refuses to be claimed rather than handing
a stranger something that can write to cameras. On a local network this is never
noticed. This is the mitigation for the obvious hazard in claim-on-first-visit,
which is that the first person to arrive owns the install.

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
non private source address is refused; zero cameras is valid; and the first save
actually creates the file.

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
