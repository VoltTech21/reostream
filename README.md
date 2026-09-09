# reostream

A bridge that pulls video from Reolink cameras over their native Baichuan protocol and
serves it as MPEG-TS over HTTP.

```
camera --Baichuan(9000)--> reostream --HTTP MPEG-TS--> Frigate / mpv / VLC
```

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
reported since 2022. reostream is a fresh implementation rather than a fork.

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

`extern` is `externStream` on the wire, 896x512 H.264 at roughly 1 Mbps. It is not
documented by Reolink and Neolink never implemented it. It sits between the 4K main
stream and the sub thumbnail, and being H.264 rather than HEVC it avoids the browser
codec problem that main runs into. See docs/measurements.md for the four-camera
measurement.

## Configuration

```toml
listen = "0.0.0.0:8560"

[[camera]]
name = "driveway"
address = "192.0.2.50"
username = "admin"
password = "$REOSTREAM_DRIVEWAY_PASSWORD"
streams = ["main", "sub"]
```

A `password` beginning with `$` is read from that environment variable. Unknown keys are
an error at startup rather than a warning, so a typo fails at boot instead of serving 404s
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
`$ENVVAR` password references and unknown keys rejected at startup rather than ignored.
Each stream is one supervised goroutine: backoff starts at 1s, doubles up to a 15s
ceiling, and resets once a connection has stayed up for more than a minute, so a flaky
camera does not carry a long backoff into an unrelated later failure. A restart never
overlaps the connection it is replacing. Audio is carried on its own PID with the same
clock as video. A slow client is disconnected rather than buffered; verified against a
live camera on a 280 kbps stream, dropped at about 65 seconds with memory flat
throughout.

Measured against a live 8 camera fleet carrying every stream those cameras have: 23
streams, 69 Mbps, about 60% of one CPU core, 29-59 MB resident. A live 4K HEVC main
stream gave 1498 frames in 60.04 seconds, 24.95 fps against a 25 fps camera, zero decode
errors. See docs/measurements.md for the full numbers.

A 7 hour soak over all 23 streams held 23/23 connected with zero restarts, zero dropped
clients and zero dropped audio frames, and resident memory oscillated inside a 29-59 MB
band rather than climbing. That band matters more than its width: an unbounded buffer is
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
- No Docker image is published yet, though a Dockerfile now exists; see Deployment
  below.

See `docs/design/` for the design and `docs/protocol.md` for what the wire actually
does, which differs from the published documentation in several places.

## Camera control

`reocam` is a second binary in this repository that reads and changes camera settings.
Streaming does not need it and does not use it.

Start here, on any camera:

```
reocam -address 192.0.2.50 -password secret probe
```

`probe` sends every read this tool knows and reports what the camera answered, because
no table can say what a given model implements. A message a camera does not have comes
back 405 rather than failing the connection, so asking is safe and is the only honest
way to find out. On one 8 MP wired camera here: 103 asked, 39 supported, 10 wanting
parameters, 54 absent.

```
reocam -address ... get all          # sweep every readable block
reocam -address ... get md           # one block, as XML
reocam -address ... set 45 < md.xml  # write a block back
reocam -address ... floodlight motion
```

The message ids come from a dispatch table inside Reolink's own firmware, recovered
statically rather than from a packet capture: 246 ids with the names the firmware gives
them, in `docs/msgids.json`. Eight of them are confirmed against live cameras, which is
what makes the rest credible. The table is explicitly not exhaustive, and a camera
dispatches with a switch rather than a table, so ids it accepts that no NVR sends will
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
config all take a changed field and read it back. `md set` and `floodlight set` answer
200 and change nothing. See `docs/control.md`.

`docs/control.md` has the rest: how the id table was recovered, how to recover more, and
the worked examples.

## Deployment

A multi-stage `Dockerfile` builds a static binary and runs it from a minimal base image
as a non-root user; see `docker-compose.yml` for an example that mounts the config file
and passes a password through the environment. Neither the config nor any credential is
baked into the image.

For a plain Linux host, `contrib/reostream.service` is a systemd unit. Read the comment
on `KillSignal` and `TimeoutStopSec` before changing either: a shutdown that does not
give the daemon time to send its stream-stop messages leaves every camera in the fleet
refusing new connections for minutes.

## Licence

AGPL v3. Free to use, run and modify. If you distribute a modified version or run one as a
service for other people, your changes have to be published too.

A commercial licence without that obligation is available. See `COPYRIGHT`.
