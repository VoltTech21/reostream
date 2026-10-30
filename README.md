# reostream

A camera proxy for Reolink cameras. It sits between your cameras and Frigate (or any
NVR), pulls video over Reolink's own Baichuan protocol, and serves it as RTSP and HTTP
MPEG-TS.

It does not replace Frigate. Frigate still does detection and recording; reostream only
changes where Frigate gets the video from.

```
Reolink cameras --Baichuan--> reostream --RTSP--> go2rtc / Frigate / Blue Iris
                                        --HTTP--> mpv / VLC / scripts
```

## Why

Reolink's RTSP server is broken on some models. On a Duo 3 it gave green blocks, gray
frames and smearing. Same camera, same encoder, same thirty minutes, both transports:

| transport | segments | segments with bitstream errors |
|---|---|---|
| Baichuan | 184 | 0 |
| RTSP | 178 | 147 |

Reolink's own NVR looks fine because it uses Baichuan, not RTSP. A fisheye here also loses
about 21% of its frames over RTSP. Method in `docs/measurements.md`.

Neolink did this first and made the protocol understandable. It has had no commits since
January 2025. reostream is a new implementation, not a fork.

## Quickstart

```
docker run -d --name reostream --restart unless-stopped \
  --network host \
  -v reostream-data:/data \
  ghcr.io/volttech21/reostream:latest

docker logs reostream
```

Host networking lets reostream see your real network: it finds cameras on its own
(battery ones too), and cameras can upload clips to it. Where you can't use it, map the
ports instead: `-p 8560:8560 -p 8561:8561 -p 8562:8562 -p 2121-2131:2121-2131`.

1. The log prints a one-time token. Open `http://<host>:8562/claim` and paste it.
2. Keep the password it gives you.
3. Add cameras: the page searches your network and lists what it finds. Enter a password
   for each one you want. Port 9000 on a wired camera must be reachable; it is on by
   default.

Your streams are at `rtsp://<host>:8561/<camera>` and `http://<host>:8560/<camera>.ts`.

On Unraid, install **reostream** from Community Apps. Prefer compose, a plain binary or systemd? See `docs/setup.md`.

![Status page](docs/images/status.png)

## Using it with Frigate

Feed go2rtc from reostream's RTSP, and point Frigate at go2rtc. One camera connection
then serves detect, record and live view.

```yaml
go2rtc:
  streams:
    driveway:
      - rtsp://reostream:8561/driveway
      - ffmpeg:driveway#audio=opus
    driveway_sub:
      - rtsp://reostream:8561/driveway_sub

cameras:
  driveway:
    ffmpeg:
      inputs:
        - path: rtsp://127.0.0.1:8554/driveway_sub
          input_args: preset-rtsp-restream
          roles: [detect]
        - path: rtsp://127.0.0.1:8554/driveway
          input_args: preset-rtsp-restream
          roles: [record, audio]
```

The `#audio=opus` line is optional; it gives browsers and apps audio in live view.

Without go2rtc, Frigate can read `http://reostream:8560/driveway.ts` directly.

The streams are open to your network by default. To ask for a login, set `username` and
`password` under `[rtsp]` (it covers ports 8560 and 8561) and use
`rtsp://user:password@reostream:8561/driveway`; see `docs/setup.md`.

## Endpoints

| RTSP, port 8561 | HTTP, port 8560 | |
|---|---|---|
| `/<cam>` | `/<cam>.ts` | main stream |
| `/<cam>_sub` | `/<cam>_sub.ts` | sub stream |
| `/<cam>_extern` | `/<cam>_extern.ts` | extern: the middle stream (about 896x512 H.264) most clients never offer |

Also on port 8560:

| | |
|---|---|
| `/<cam>.keyframe` | latest keyframe, raw |
| `/<cam>.frames` | raw access units with a small header, for your own decoder; see `docs/setup.md` |
| `/<cam>.still.ts` | the latest keyframe as a one-frame MPEG-TS; never touches the camera |
| `/api/status` | per-stream JSON: connected, fps, bitrate, clients, reconnects, last error |
| `/metrics` | the same for Prometheus |

## What it does

- One connection per stream, held open whether anyone is watching or not.
- Timestamps from the camera's own clock, so frame rates come out right.
- A slow client is disconnected, never buffered. No client can stall a camera.
- New clients start at a keyframe. Audio is carried when the camera sends it.
- Two-way audio: on cameras with a speaker, Frigate's talk button works through it.
- The camera's own detections (motion, person, vehicle, animal) go to MQTT and to Frigate
  as events, so a small box can lean on the camera instead of decoding everything.
- Recordings from the camera's SD card, and a small FTP receiver that keeps a copy of each
  clip the camera uploads.
- Battery cameras: found by their own UDP search, woken only while someone is watching,
  battery level shown without waking them.
- Sessions are closed cleanly. A camera left with an open session refuses connections
  for minutes.
- A web page on port 8562 for status, live view, config, logs and camera settings.

No ffmpeg, GStreamer or RTSP server in the serving path.

## Tested on

On my own cameras:

| Model | Count | Firmware |
|---|---|---|
| RLC-810A (8 MP bullet) | 6 | v3.1.0.956_24022103 |
| FE-P (fisheye) | 1 | v3.0.0.5336_2509021915 |
| Duo 3 PoE (dual lens panorama) | 1 | v3.0.0.5049_2506302188 |
| TrackMix PoE (PTZ, auto tracking) | 1 | v3.2.0.5931_2601294931 |
| Argus PT Ultra (battery, Wi-Fi) | 1 | v3.0.0.5808_26041006 |

reostream has been the only video path for this fleet's Frigate since September: 23
streams, 69 Mbps, about 60% of one CPU core, 29-59 MB of memory. Numbers in
`docs/measurements.md`.

Reported working by users (thank you!):

| Model | What works | Reported in |
|---|---|---|
| RLC-833A (v3.1.0.3016) | video, two-way talk | GitHub #18 |
| E1 Pro (v3.1.0.1414) | video, two-way talk | GitHub #18 |
| E1 | video, privacy mode | GitHub #13 |
| E1 Zoom 8 MP | video, two-way talk, pan/tilt | Reddit |
| RLC-822A (6 cameras) | video | GitHub #1 |
| Video Doorbell PoE (D340P) | video | GitHub #1 |
| Video Doorbell (wired 2K) | video | Reddit |
| Video Doorbell Wi-Fi | video | Reddit |
| CX820 | video | Reddit |
| Duo Floodlight Wi-Fi v2 | video | Reddit |
| Duo 4K Wi-Fi (3 cameras) | video | Reddit |
| RLC-811A (v3.1.0.3485) | video (sub and extern) | Reddit |
| Lumus Pro | video (Frigate with hardware decoding off for it) | GitHub #17 |

Your camera not here? Open an issue with the model, firmware and what worked,
and it goes on the list.

## Camera control

Each camera has a page with three tabs:

- **Live:** the player, stream picker, pan/tilt with presets, talk, snapshot, floodlight,
  and every stream URL to copy.
- **Recordings:** clips from the SD card on a timeline, plus clips saved by reostream.
- **Settings:** image, name and timestamp, encoding, lights, audio, pan/tilt (auto return,
  auto tracking), FTP, email and push alerts, network services, date and time, and every
  raw value for anything not covered. Instant settings save as you change them; ones that
  restart the stream or the camera ask first. A blue dot marks a setting changed from the
  camera's factory value, with a button to put it back. Settings can be copied to other
  cameras.

![Camera page](docs/images/camera.png)

`reocam`, a separate command line tool, reads and changes camera settings over the same protocol. Streaming does not
need it.

```
reocam -address 192.0.2.50 -password secret probe
```

`probe` reports which messages the camera answers. See `docs/control.md` for reading
and writing settings and what to watch out for.

## More

- `docs/setup.md`: other ways to run it, the config file, the control page, RTSP, the frames feed,
  recordings and saved clips, alerts, battery cameras
- `docs/control.md`: camera control with `reocam`
- `docs/protocol.md`: what the Baichuan wire actually does
- `docs/measurements.md`: how every number above was measured

## License

AGPL v3. Free to use, run and modify. If you distribute a modified version or run one as a
service for others, you must publish your changes.

A commercial license without that obligation is available. See `COPYRIGHT`.
