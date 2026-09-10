# The operator page

A web surface served by the reostream daemon for the three things that today
require a text editor, `curl` and `docker logs`: configuring it, confirming it
works, and reading its logs. Plus a first run flow that takes someone who has
never seen this project from an empty config to working URLs pasted into their
NVR.

## Why this exists, and why it stops where it does

Everything reostream can tell you already exists. `/api/status` carries per
stream connected, streaming, fps, bitrate, keyframe age, clients, restarts and
last error, and `/metrics` carries the same. Nothing renders it. The question an
operator asks is whether a camera is streaming right now, and answering it means
reading JSON by hand.

Configuration is a hand edited TOML file. Setup is building an image yourself and
composing a long `docker run`. Logs are `log.Printf` to stderr with no history,
no filter and no way to see them from anywhere but a shell on the host.

This document covers one half of a larger surface. The other half, changing
settings on the cameras themselves, is a separate project built on `cmd/reocam`,
and is deliberately not in here. The split is by write target:

- **reostream's own operator page**, this document. Reads status, edits
  reostream's config, shows reostream's logs. It never writes to a camera.
- **The camera control surface**, later. Settings, PTZ, fisheye sections, dual
  lens stitch. It writes to cameras, it is much larger, and a fault in it must
  never be able to take video down.

Both halves can look like one application to a user. Only one of them runs inside
the streaming daemon.

## Placement

A new `internal/control` package serving its own listener, configured the way
`[rtsp]` already is:

```toml
[control]
listen = "0.0.0.0:8562"
password = "$REOSTREAM_CONTROL_PASSWORD"
```

Absent the section nothing is served and nothing changes. Present with `listen`
and no password, the daemon refuses to boot unless `allow_no_password = true` is
set explicitly.

The streaming port keeps its current behaviour exactly: open, unauthenticated,
and what Frigate points at. Control is a separate socket with its own auth, so a
recorder never has to hold a credential and the two concerns cannot be confused
in a firewall rule.

Login is a password only, no usernames, compared in constant time, with a session
cookie afterwards. Templates and assets are compiled in with `embed`, so
deployment is still one binary.

Server rendered Go templates with plain JavaScript. No build step, no node
toolchain, no framework. The only vendored dependency is a JavaScript MPEG-TS
player, and it is needed under any approach. If the camera control surface later
proves this too limiting it can be built as a client against the same JSON,
without discarding this.

## Status

One row per stream, from the `server.StreamStatus` data that already exists.

Raw fields are translated into a state, because the distinction between
`Connected` and `Streaming` is the entire reason that struct is shaped the way it
is, and a newcomer will not infer it:

| state | meaning |
|---|---|
| Streaming | connected, frames arriving |
| No video | connected, no frame for longer than the media watchdog's interval |
| Reconnecting | with backoff and restart count |
| Down | with `LastError` shown verbatim |

"No video" is the held session signature, and it is the failure that left two
streams unnoticed for 25 seconds in the 2026-09-08 incident. It is a distinct
state on this page rather than a subtlety of a number.

## The picture

Live video tiles, playing the sub stream from the existing HTTP endpoint through
a JavaScript MPEG-TS player, decoded by the browser.

This costs nothing. The hub is a broadcaster, so a browser tile is one more HTTP
subscriber to a stream that is already open, with no additional camera connection
and no additional camera load. A tile that cannot keep up is disconnected rather
than buffered, which is what the serving path already does for every client.

An earlier version of this design took snapshots from the cameras instead. It was
wrong in both directions: it would have opened a second connection to a camera
whose main stream tolerates one, and it would have needed an HEVC decoder in a
daemon that deliberately has none, since `/<cam>.keyframe` is documented as no
decode in the serving path. A still image, where one is wanted, is a paused video
element.

Fallbacks, in order:

1. Sub stream, where the browser can decode it.
2. `extern`, confirmed H.264 at 896x512 and roughly 1 Mbps, for models whose sub
   stream the browser will not play.
3. The camera's own CGI `Snap`, which returns a JPEG over HTTP with no Baichuan
   session and no stream slot, for anything neither of the above covers.

Main streams are labelled as HEVC and not playable in a browser rather than
failing silently.

## Configuration

A form for what is actually changed: top level `listen`, `[rtsp]`, `[control]`,
and per camera name, address, username, password, which streams are served and
which of those are served over RTSP. A raw TOML view sits behind it for
everything the form does not cover, so no option is ever unreachable.

Validation runs `internal/config`, the same parser the daemon boots with, not a
second implementation of it. Unknown keys are already a hard error there, so a
typo is caught in the browser rather than at boot. An invalid config is never
written.

Writes are atomic, temp file and rename, keeping the previous file as a backup.

Existing passwords render masked and are write only. The form can replace one and
can never display one. It steers toward the `$ENVVAR` references the config
already supports.

## Reload

Saving diffs the old config against the new and sorts every stream into added,
removed, changed or unchanged. Unchanged streams are not touched. Adding a ninth
camera does not disturb the other eight, and a recorder sees no interruption.

Two rules the diff has to respect:

**A stopped stream releases its session before the same camera is restarted.** A
camera whose session is not released refuses new connections for several minutes.
A changed camera is stopped to completion and then started, never overlapped.

**Listener changes are not hot applied.** The listen address, the RTSP port and
the control port require a restart. The page says so plainly instead of saving
the value and quietly not applying it.

This is an explicit `Reload` on the supervisor with its own tests, not a second
call to `Run`. `Run` was not idempotent and duplicated every stream goroutine, and
a careless implementation of this feature recreates that bug exactly.

## Logs

A tee on the standard logger at startup, keeping the last 2000 lines in an in
memory ring buffer. stderr behaviour is unchanged, so `docker logs` and journald
continue to work as they do now.

The page live tails that buffer over server sent events, filterable by camera
name. Log lines are already prefixed `reostream: <name>:`.

No log file and no rotation. The container or the service manager owns that, and a
daemon that writes its own logfile brings a class of disk full failures this does
not need.

## First run

With no cameras configured, the control page opens on setup rather than the
dashboard. It is the same forms with a few extra templates, not a separate
subsystem.

1. **Add a camera.** Address, username, password. The page connects, logs in and
   reports what the camera is: model, the streams it has, their resolutions and
   codecs. A wrong password becomes "authentication failed" rather than the empty
   login reply that reads like a held session.
2. **Choose streams**, with codecs shown, so choosing between `main` and `extern`
   is informed.
3. **Choose outputs.** HTTP always. RTSP a toggle.
4. **Take the URLs.** Copy blocks for Frigate YAML, RTSP URLs for other
   recorders, and a plain URL for VLC.
5. **Watch it work.** Save, reload, and the live tile comes up. The question "is
   it working" is answered by watching it work.

## Camera discovery

Wanted, and sequenced so it cannot block the page.

There is no discovery today. The message id dispatch table recovered from
firmware is TCP Baichuan and does not describe it, so the local UDP device search
is unrecovered protocol work: either a capture of the official client's add
device flow through `bcpcap`, or another pass through firmware. That method has
worked before, which is the reason to expect this to land, not a reason to depend
on it.

Manual add is built regardless, and is needed regardless: discovery finds cameras
on the local segment, it will not find one across a VLAN, and it cannot supply
credentials. Discovery, when it lands, adds a "found on your network" list to step
one of setup and changes nothing else.

## Verification

`internal/fakecam` exists, so reload is tested rather than eyeballed. Add, remove
and change cameras while streams run, and assert that untouched streams keep their
connections and that the goroutine count does not grow.

Config round trip, validation rejection and auth are unit tested. Browser code is
kept thin so that the logic worth testing is in Go.

Before this is called done, the rule the rest of this project runs on applies:
measure the effect, not the call. Reload one camera on the live fleet and confirm
from `/api/status` that the other streams show zero restarts and no interruption.

## Not in this document

- Camera settings, PTZ, fisheye view modes and dual lens stitch. Separate project
  on `cmd/reocam`.
- Any write to a camera. This page does not make one.
- Recording, playback or events. reostream does not do these and this does not
  change that.
- Multiple users, roles or an audit log. One password.

## Follow up

The README argues for reostream partly by what it refuses to contain, naming
GStreamer and ffmpeg. That framing predates the product this is becoming and
should be revisited as prose, separately from this work.
