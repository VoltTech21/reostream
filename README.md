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

Measured against a live 8 camera fleet: 16 streams, 40.2 Mbps, about 14% of one CPU
core, 16 MB resident. A live 4K HEVC main stream gave 1498 frames in 60.04 seconds,
24.95 fps against a 25 fps camera, zero decode errors. See docs/measurements.md for the
full numbers.

Not yet done, and worth being direct about:

- It has run for hours at a time, not weeks. No long soak has completed.
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
