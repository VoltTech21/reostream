# Pan and tilt

reostream can move a pan/tilt camera for Frigate. Frigate steers cameras over ONVIF, and a
Reolink that only speaks Baichuan (the E1 has no RTSP and no ONVIF of its own) has no
ONVIF to offer, so reostream serves a small ONVIF endpoint of its own and turns what
Frigate asks into Baichuan messages.

Video is unaffected. This is a separate session to the camera, opened when someone moves
it and closed after 30 seconds of quiet.

## Turn it on

One port per camera. Frigate reaches ONVIF at a fixed path on the host and port it is
given, so the port is what tells reostream which camera is meant.

```toml
[[camera]]
name = "nursery"
address = "192.0.2.50"
username = "admin"
password = "$NURSERY_PASSWORD"
streams = ["main", "sub"]
rtsp = ["main", "sub"]
onvif = "0.0.0.0:8656"
```

Then in Frigate:

```yaml
cameras:
  nursery:
    onvif:
      host: reostream
      port: 8656
```

The endpoint has no login unless you give it one, so bind it to a trusted interface
(`127.0.0.1` when Frigate shares the host) or set one:

```toml
onvif_user = "frigate"
onvif_password = "$NURSERY_ONVIF_PASSWORD"
```

and give Frigate the same `user` and `password`. These belong to the endpoint, they are not
the camera's. Both accept `$NAME` like the camera's own fields.

The endpoint starts with the process. Changing `onvif` on the config page is saved but
needs a restart, and the page says so.

## What works

| Frigate | reostream |
|---|---|
| move left, right, up, down | `PtzControl` left, right, up, down; runs until stopped |
| stop | `PtzControl` stop |
| presets in the UI and `/api/<camera>/ptz/info` | the camera's preset list |
| go to preset | `PtzPreset` toPos |
| move status | reostream's own: moving from a move until its stop |

Things to know:

- **No diagonals.** A command like `leftUp` is answered 200 by an E1 and does nothing, so a
  velocity with both axes set moves along the stronger one. Frigate only sends straight
  moves.
- **Speed.** The E1 moves at one fixed speed whatever the number sent (speeds 1 to 100
  moved it the same distance in the same time), so the ONVIF velocity is passed through
  and the camera ignores it. A model that honours it will.
- **A move that is never stopped is stopped for you** after 15 seconds, so a client that
  dies mid-press cannot leave the motor against the end stop. A `Timeout` on a
  ContinuousMove is honoured, up to 60 seconds.
- **No zoom, focus, absolute or relative moves, home position, or saving presets.** None is
  advertised, so clients do not try. The E1 has no zoom motor. Autotracking needs relative
  moves and so is not available.
- **Presets** are listed with the camera's own names. Save them in the Reolink app.

## Checked against

A Reolink E1 (firmware v3.2.0.4858), pan/tilt, no zoom, with Frigate's own ONVIF client
library (`onvif-zeep-async`): discovery, profile selection, presets, move, stop, status and
go-to-preset, with and without a login. The pan counter (`ptzcurpos`, message 433) moved
with each command and came back with the opposite one.

Other models are untested. In particular a model with zoom, or one that moves at variable
speed or in diagonals, would need to be looked at before more is advertised.

## What goes over the wire

Message 18 (`PtzControl`) carries `<speed>` and `<command>`, one of `left`, `right`, `up`,
`down`, `stop`. Message 19 (`PtzPreset`) carries a preset id and `toPos`. The preset list
is read with message 190; the same read on 19 answers 200 with nothing. Both writes are
two section messages like every other write. See `internal/baichuan/ptz.go`.
