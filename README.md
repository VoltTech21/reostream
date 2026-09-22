# reostream

A bridge that pulls video from Reolink cameras over their native Baichuan protocol and
serves it as MPEG-TS over HTTP.

```
camera --Baichuan(9000)--> reostream --HTTP MPEG-TS--> Frigate / mpv / VLC
```

## Quickstart

```
docker run -d --name reostream \
  -v reostream-data:/data \
  -p 8560:8560 -p 8562:8562 \
  ghcr.io/volttech21/reostream:latest

docker logs reostream
```

The log prints a one-time token. Open `http://<this-host>:8562/claim`, paste it,
take the password it offers you, and add a camera. That is the whole setup; there
is no config file to write first and no password to choose in advance.

Point your recorder at `http://<this-host>:8560/<camera>.ts`.

If you want to read why any of this exists before running it, carry on below.
Configuration has the same four steps in more detail.

## Why

Reolink cameras have an RTSP server, and on some models it does not work properly. On a
Duo 3 here it produced green blocks, flat grey frames and purple smearing. Capturing both
transports from the same camera for thirty minutes and comparing identical wall-clock
seconds showed the problem is the RTSP server itself, not the encoder:

| transport | segments | segments with bitstream errors |
|---|---|---|
| Baichuan | 184 | 0 |
| RTSP | 178 | 147 |

Same camera, same encoder, same second. That is also why the manufacturer's own NVR looks
fine: it uses Baichuan and never touches the RTSP server.

Method and the rest of the measurements are in docs/measurements.md.

Reolink's battery cameras have no RTSP at all, so Baichuan is the only way to get video
off them, and a fisheye model here loses about 21% of its frames over RTSP.

Neolink solved this first and deserves the credit for making the protocol legible. It has
not had a commit since January 2025 and has 124 open issues, including a memory leak
reported since 2022. reostream is a fresh implementation, not a fork.

## What it does

- One long-lived connection per stream, held whether or not anyone is watching.
- Real presentation timestamps taken from the camera's own clock. An elementary stream
  carries no timing of its own, so anything that muxes it without this invents timestamps
  and produces wrong frame rates.
- Broadcast fan-out. A client that stops reading is disconnected, never buffered. No
  client can slow down or wedge a camera.
- New clients join at the next keyframe, after being handed the cached PAT and PMT.
- Audio muxed onto its own stream where the camera provides AAC.
- Clean session shutdown, which matters: a camera whose session is not released refuses
  new connections for several minutes.

There is deliberately no RTSP server, no GStreamer, no session pool and no ffmpeg in the
serving path. Every bug worth avoiding here lived in one of those.

## Streams

Reolink cameras carry three streams, not the two the app shows. `extern` is the
undocumented middle option, typically 896x512, which is useful when the main stream is 4K
and the substream is too small to be worth looking at.

| Endpoint | Stream |
|---|---|
| `GET /<cam>.ts` | main |
| `GET /<cam>_sub.ts` | sub |
| `GET /<cam>_extern.ts` | extern |
| `GET /<cam>.keyframe` | most recent keyframe, raw elementary stream, no decode in the serving path |
| `GET /api/status` | per-camera JSON: connected, fps, bitrate, keyframe age, clients, reconnects |
| `GET /metrics` | the same in Prometheus format |

## RTSP

RTSP is served alongside HTTP, not instead of it, for software that takes an
RTSP URL and nothing else.

    [rtsp]
    listen = "0.0.0.0:8554"

    [[camera]]
    name = "driveway"
    streams = ["main", "sub"]
    rtsp = ["main"]

| Endpoint | Stream |
|---|---|
| `rtsp://host:8554/<cam>` | main |
| `rtsp://host:8554/<cam>_sub` | sub |
| `rtsp://host:8554/<cam>_extern` | extern |

Absent the `[rtsp]` section nothing is served and every stream keeps the same
path it has always had.

TCP interleaved is the default transport. UDP is offered for clients that
insist, and is not preferred: a lost RTP packet is a corrupt frame with no
retransmit, which is worse than the larger header interleaving costs.

**RTSP is not an upgrade to the HTTP output.** HTTP is stateless, one long
lived GET, and MPEG-TS resynchronises itself, so a client that loses the
connection reconnects with a single request. RTSP carries session state that
a network blip can leave half open, and reconnecting costs a full handshake.
What RTSP is better at is telling a client that a stream has died, through
RTCP and keepalives, where a stalled TS stream looks alive until the
consumer's own timeout fires. Running both keeps both properties.

`extern` is `externStream` on the wire, 896x512 H.264 at roughly 1 Mbps. It is not
documented by Reolink and Neolink never implemented it. It sits between the 4K main
stream and the sub thumbnail, and being H.264 not HEVC it avoids the browser
codec problem that main runs into. See docs/measurements.md for the four-camera
measurement.

## Configuration

There is nothing to write by hand before the first run. A fresh install starts with no
config file at all:

1. Start the container against an empty data directory (`docker compose up`, or `docker
   run` with `-v data:/data`). It comes up serving no cameras and prints a one-time claim
   token to its own log.
2. Read the token with `docker logs` (or `docker compose logs`). It looks like
   `XXXX-XXXX-XXXX-XXXX`, and the log line names the URL to open. The token lives in
   memory only: restarting the container before it is claimed prints a new one and the
   old one stops working.
3. Open that URL and enter the token. The password field arrives already filled in with
   a password generated for that render, 16 characters of `crypto/rand` from a
   31-character alphabet, 79.3 bits. Accepting it as it stands is the recommended
   answer: it is the only thing that makes the password strong by construction, and
   nothing on this page is rate limited. A password you type instead must be at least 16
   characters. Either way, submitting writes `/data/config.toml` inside the container and
   the page starts requiring that password from then on.
4. Add a camera from the page, or by editing the config directly, either by hand on the
   mounted data volume or through the page's own editor.

   If you edit by hand, **add** the `[[camera]]` block to the file that is already
   there; do not replace the file with this fragment. A config with no `[control]`
   section is treated as not yet claimed. That is deliberate, and it is what stops a
   half-written file leaving the page open to anyone, but it means overwriting your
   config with the block below would send you back to the claim screen with no way in
   except editing the file again.

```toml
listen = "0.0.0.0:8560"

# ... your existing [control] section stays exactly as it is ...

[[camera]]
name = "driveway"
address = "192.0.2.50"
username = "admin"
password = "$REOSTREAM_DRIVEWAY_PASSWORD"
streams = ["main", "sub"]
```

A `password` beginning with `$` is read from that environment variable. Unknown keys are
an error at startup, not a warning, so a typo fails at boot instead of serving 404s
at three in the morning.

## Using it with Frigate

Point Frigate at the HTTP endpoint. No go2rtc entry is needed for detect or record.

```yaml
cameras:
  driveway:
    ffmpeg:
      inputs:
        - path: http://reostream:8560/driveway_sub.ts
          roles: [detect]
        - path: http://reostream:8560/driveway.ts
          roles: [record, audio]
```

go2rtc is still worth keeping for Frigate's browser live view, because that needs MSE or
WebRTC and browsers cannot play HEVC over WebRTC. Detect and record do not go through it.

## Status

The daemon runs from a TOML config file, one `[[camera]]` block per camera, with
`$ENVVAR` password references and unknown keys rejected at startup instead of ignored.
Each stream is one supervised goroutine: backoff starts at 1s, doubles up to a 15s
ceiling, and resets once a connection has stayed up for more than a minute, so a flaky
camera does not carry a long backoff into an unrelated later failure. A restart never
overlaps the connection it is replacing. Audio is carried on its own PID with the same
clock as video. A slow client is disconnected, never buffered; verified against a
live camera on a 280 kbps stream, dropped at about 65 seconds with memory flat
throughout.

Measured against a live 8 camera fleet carrying every stream those cameras have: 23
streams, 69 Mbps, about 60% of one CPU core, 29-59 MB resident. A live 4K HEVC main
stream gave 1498 frames in 60.04 seconds, 24.95 fps against a 25 fps camera, zero decode
errors. See docs/measurements.md for the full numbers.

A 7 hour soak over all 23 streams held 23/23 connected with zero restarts, zero dropped
clients and zero dropped audio frames, and resident memory oscillated inside a 29-59 MB
band instead of climbing. The band matters more than its width: an unbounded buffer is
the failure this project exists to avoid.

Not yet done, and worth being direct about:

- Seven hours is not seven weeks. No multi-day soak has completed.
- Tested against one fleet: four camera models, one firmware generation. Nobody else's
  cameras have been tried.
- **Battery cameras have never been tested.** This is the biggest gap on this list.
  Battery models are the main reason most people reached for Neolink in the first
  place, since they have no RTSP server at all, and they behave differently from wired
  cameras: they sleep, wake on motion, and send battery-state messages the protocol
  client has never seen.
- Container images are built and pushed only when a release is tagged, so a commit on
  main can be ahead of `:latest`. See Deployment below for building your own.

See `docs/design/` for the design and `docs/protocol.md` for what the wire actually
does, which differs from the published documentation in several places.

## Control

An `[control]` section turns on a web page for the three things that otherwise need a
text editor, `curl` and `docker logs`: watching stream status, editing the config, and
reading logs.

```toml
[control]
listen = "0.0.0.0:8562"
password = "$REOSTREAM_CONTROL_PASSWORD"
```

Absent the section this daemon starts no control listener of its own. That is not the
same as "nothing changes": a container image started with no config at all serves the
page anyway, on 0.0.0.0:8562, so there is somewhere to claim the install from; see
Configuration above. While a config file exists but names no `[control]` password the
page stays behind its claim screen and serves nothing else, so an old config carried
over from before this feature is closed, not open. Write a `[control]` section
with a password into it and the running daemon picks that up on its next request, with
no restart.

`password` follows the same rule as a camera password: a value beginning with `$` is
read from that environment variable. `listen` with no password refuses to boot unless
`allow_no_password = true` is set explicitly.

Control runs on its own listener, separate from the streaming port. The streaming port
stays open and unauthenticated, which is what a recorder needs; the control port carries
the only credential in this daemon, so a firewall rule that opens streaming to a recorder
never has to also decide whether that recorder should be able to change the config.

The page shows one row per stream translated from `/api/status` into a state
(streaming, no video, reconnecting, down) instead of raw booleans, plays each stream's
video live in the browser, edits the config with the same parser the daemon boots with
so an invalid save is rejected before it is written, and tails the daemon's own logs.

It also controls the cameras themselves, which is the other half of what it is for.
Each camera has its own page: what the camera says it is and what this login may do on
it, what every read this daemon knows answers, a curated set of settings that can
actually be changed, the floodlight, its NTP server and timezone, and an advanced view
of every readable block with the raw write behind it. Nothing there is presented as more
certain than it is: each writable pair carries a confidence, one of proven, unverified, known
inert or unsafe to rewrite, taken from what was actually observed on a camera and not
from a 200, and a write that answers 200 is reported as "accepted" and not as "confirmed"
unless it was read back. NTP and timezone can be applied to the whole fleet at once,
per-camera result by per-camera result. See docs/control.md.
Saving diffs the old config against the new and reloads only what changed: an unchanged
stream is never stopped, and a changed camera is stopped to completion before it is
started again, so its session is released before the same camera is asked to reconnect.
Verified on a live 8 camera fleet; see docs/measurements.md.

Two things about deploying it matter more than they look:

- **The container runs as a non-root user (uid 65532).** If the data directory is owned
  by root, or not writable by that user, the very first save fails: with no config yet,
  that first save is the claim itself. The image creates `/data` owned by that user, and
  a named volume (the pattern in docker-compose.yml) inherits that ownership on first
  use; a bind mount to a host directory needs to be writable by uid 65532 yourself, for
  example `chown -R 65532:65532` on that directory before the first run.
- **Mount the data directory, not a single config file.** A single-file bind mount
  (`./config.toml:/data/config.toml`) leaves the directory around it not writable from
  inside the container, so no temp file can be created beside it and the atomic rename
  this daemon otherwise uses is unavailable. Mounting the whole directory (`./data:/data`,
  or the named volume in docker-compose.yml) restores atomic replacement and keeps the
  backup durably next to the config across restarts.

## Camera control

`reocam` is a second binary in this repository that reads and changes camera settings.
Streaming does not need it and does not use it.

Start here, on any camera:

```
reocam -address 192.0.2.50 -password secret probe
```

`probe` sends every read this tool knows and reports what the camera answered, because
no table can say what a given model implements. A message a camera does not have comes
back 405 instead of failing the connection, so asking is safe and is the only honest
way to find out.

Across three models here, a fixed 8 MP bullet, a fisheye and a dual lens pano, 89 of 103
messages answer identically. The 14 that differ are the ones you would expect: fisheye
and stitching messages on the models that have those lenses, the smart detection suite on
the newest one. `docs/control.md` has the matrix.

```
reocam -address ... get all          # sweep every readable block
reocam -address ... get md           # one block, as XML
reocam -address ... set 45 < md.xml  # write a block back
reocam -address ... floodlight motion
```

The message ids come from a dispatch table inside Reolink's own firmware, recovered
statically, not from a packet capture: 246 ids with the names the firmware gives
them, in `docs/msgids.json`. Eight of them are confirmed against live cameras, which is
what makes the rest credible. The table is explicitly not exhaustive, and a camera
dispatches with a switch and not a table, so ids it accepts that no NVR sends will
not appear.

**Never write a document this tool invented.** The only correct body for a write is what
the matching read returned with one field changed. The camera supplies its own schema
that way, including fields no camera here has, and echoing its document back preserves
the exact byte formatting some messages insist on. There are 54 read/write pairs.

Three things will mislead you:

- **421 means the message was built wrong, not that the model lacks it.** A
  configuration write is a two section message, the channel in an extension and the
  document in a second section. Sent as one section a camera answers 421 and changes
  nothing.
- **A reply status is not proof.** Message 288 accepts a floodlight command, answers
  200, and does nothing observable. Two way audio "worked" twice before it made a sound.
  Verify the effect, not the call.
- **Some settings are HTTP only on this firmware.** The floodlight, the fisheye view
  modes and the dual lens stitch parameters all go over the camera's CGI API, not
  Baichuan.

`reocam verify -write` writes each document straight back unchanged, which exercises the
write path without altering the camera. All 20 applicable pairs on one wired camera
returned 200 with the document unchanged.

Acceptance is not effect, though, and they come apart per message: OSD, LED and email
config all take a changed field and read it back. `md set` and `floodlight set` answer 200
and change nothing, `md set` on both models tried. Three writes are proven to work, two
are proven inert, and fifty are untested. See `docs/control.md`.

`docs/control.md` has the rest: how the id table was recovered, how to recover more, and
the worked examples.

## Deployment

A multi-stage `Dockerfile` builds a static binary and runs it from a minimal base image
as a non-root user; see `docker-compose.yml` for an example that mounts a data directory
and publishes the streaming and control ports. Nothing is baked into the image: a fresh
install has no config and no password until it is claimed through the control page, per
the Configuration section above.

For a plain Linux host, `contrib/reostream.service` is a systemd unit. It claims the
same way the container does: it passes `-data /var/lib/reostream`, systemd creates that
directory for it, and the config appears there when the install is claimed. Read the
comment on `KillSignal` and `TimeoutStopSec` before changing either: a shutdown that does not
give the daemon time to send its stream-stop messages leaves every camera in the fleet
refusing new connections for minutes.

## Licence

AGPL v3. Free to use, run and modify. If you distribute a modified version or run one as a
service for other people, your changes have to be published too.

A commercial licence without that obligation is available. See `COPYRIGHT`.
