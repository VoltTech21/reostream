# reostream: design

Written 2026-09-04. Supersedes the "v2" sketch in `~/projects/neolink/V2_NOTES.md`.

## What this is

A Reolink camera bridge: it speaks Baichuan to the cameras and serves MPEG-TS over
HTTP. One process, one config file, all cameras.

    camera --Baichuan(9000)--> reostream --HTTP MPEG-TS--> Frigate / mpv / VLC

It replaces the current production path, which is:

    camera --Baichuan--> neolink-cat --TS on stdout--> ffmpeg -c copy --RTSP--> go2rtc --> clients

That path works and stays in production until reostream earns each camera. For the
detect and record paths, the CPU-heavy ones and the ones where every bug lived,
reostream removes `neolink-cat`, the per-camera `ffmpeg -c copy`, and the RTSP hop.

**go2rtc is not fully removed, and the earlier claim that it was is wrong.** Frigate's
browser live view is go2rtc-powered: the config has a `webrtc:` block and a per-camera
`live:` section pointing at the `<cam>_h264` streams. Those H.264 transcodes exist
because browsers cannot play HEVC over WebRTC, which serving MPEG-TS does not change.
So go2rtc stays for live view and birdseye, fed from reostream rather than from an
exec+ffmpeg chain.

That is narrower than it sounds, and it avoids the go2rtc bugs that motivated this
project. The cam wall is mpv, which plays an HTTP-TS URL directly, so the wall bypasses
go2rtc entirely, and with it both the H.265 RTP packetisation suspected of degrading the
pano and the `pkg/h265/rtp.go` panic that breaks HLS. Frigate's detect and record paths
also bypass it. What remains is only the Frigate web UI's live tab, which needs MSE or
WebRTC. If that tab is not used much, go2rtc can be dropped entirely and Frigate falls
back to its built-in jsmpeg live view. That is a cutover-time config decision, not an
architectural one, and nothing in this design depends on which way it goes.

Frigate ingest itself is native: Frigate pipes every input through ffmpeg, so a
reostream endpoint is just a URL. The only other change is `input_args`, currently
`preset-rtsp-restream` on all 8 cameras, which becomes a small custom set with no
`-rtsp_transport`.

This is a new project, not a fork of Neolink. Neolink is abandoned upstream and its
streaming layer is where every bug we found lives. We keep what we learned from it and
from the NVR firmware; we do not keep its code.

## Non-goals

These are load-bearing. Neolink did not set out to have three resource leaks; it got
them by taking on jobs it did not need to do.

- **No RTSP server.** RTP packetisation, session management, TCP interleaving, teardown.
  H.265 RTP packetisation is exactly what we suspect go2rtc gets wrong; writing our own
  means owning that bug with none of go2rtc's field testing. TCP already delivers bytes
  in order; MPEG-TS over HTTP needs none of this machinery.
- **No GStreamer.** The appsrc state machine caused the pause deadlock.
- **No session pool.** One broadcast per camera, no per-client state beyond a socket.
- **No ffmpeg in the serving path.** Transcoding is a separate concern; if a client needs
  H.264 from an H.265 camera, that client can run its own ffmpeg.
- **No cloud/UID/P2P discovery.** Cameras are on a local network at known addresses.
- **Not in v1:** PTZ, playback (`DayRecords`/`FileInfoList`/`ReplaySeek`), AI detection
  config, MQTT, motion events, two-way audio. All are additive later.

## Language and provenance

**Go.** Reasons, in order:

1. This should be my own project, under a licence I choose. Depending on
   `neolink_core` means AGPL and a dependency on dead upstream code.
2. Go is the right tool for "hold N long-lived connections and fan bytes to HTTP
   clients". go2rtc is Go, which is proof the terrain suits the job.
3. The protocol work we actually want (`HeartBeat`, `GopCfg`, `BinoStitch`) must be
   written from wire captures regardless of language, so none of that value is lost.

Cost: the Baichuan protocol must be implemented. `neolink_core` is 14,105 lines, but
~3,400 of those are UDP/P2P discovery we will never use, and more is PTZ, talk, email
and floodlight. The part reostream needs (login handshake and its crypto, message
framing, the media depacketiser, and a small slice of the XML catalogue) is roughly
3,000–3,500 lines. Bounded, but the riskiest part of the project, which is why Phase 0
exists.

### Clean-room rules

These apply to every commit.

- Implement from `dissector/baichuan.lua`, `dissector/messages.md`, and live packet
  captures. The wire is the specification.
- Two firmware assets are **unstripped** and worth consulting to know what to look for:
  `libstreambuffer.so`, whose `sb_add_reader`/`sb_del_reader`/`sb_can_read`/`sb_get_readpos`
  API is the NVR's own per-reader ring buffer (the same shape as our fan-out), and
  `netclient`, the NVR's camera-facing Baichuan client, whose log format strings name
  things directly (`login xml:%s`, `preview start xml:%s`).
- The NVR firmware at `~/reolink-fw/app/` (`netserver`) tells us **what exists**: message
  names, XML element names, serializer symbols. Use it to know what to look for. Do not
  transcribe decompiled output into the repo.
- Do not copy Neolink source. Do not vendor `neolink_core`. Do not paste its code as a
  starting point.
- `ts.rs` from `neolink-cat` is our own work and may be ported to Go freely.

## Architecture

Four packages, each with one job and a testable boundary.

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/baichuan` | Wire protocol: connect, login, message framing, XML codec, media depacketise, ping/heartbeat. Yields decoded frames. | net, crypto |
| `internal/ts` | MPEG-TS muxer. Frames in, TS packets out. Pure, no I/O. | nothing |
| `internal/camera` | One stream's lifecycle: connect, login, start stream, heartbeat, mux, broadcast, reconnect. | baichuan, ts |
| `internal/server` | HTTP: endpoints, fan-out to clients, status and metrics. | camera |

`cmd/reostream` wires them together from config. Nothing above `baichuan` knows about
the wire; nothing below `camera` knows about HTTP.

### Layout

    cmd/reostream/main.go
    internal/baichuan/     conn.go login.go crypto.go message.go media.go
    internal/ts/           mux.go pes.go pat.go
    internal/camera/       camera.go supervisor.go
    internal/server/       server.go stream.go status.go snapshot.go
    docs/                  protocol.md (what we learned, from captures)

## Camera lifecycle

One goroutine per **stream** (a camera's main and sub are separate connections), started
at boot and running whether or not anyone is watching:

    connect -> login -> start video -> loop { read frame; mux; broadcast; ping if due }

On any error: log it, **close the connection cleanly**, back off (1s doubling to 15s),
reconnect. The camera task owns the connection for its whole life.

Three rules learned the hard way, each of which cost hours:

1. **Emit real PTS from the camera's `microseconds` field.** A raw elementary stream
   carries no timing, so anything muxing it invents non-monotonic timestamps. The
   symptoms were a 216fps transcode and mpv dropping 10,387 frames. Camera time is
   microseconds; MPEG-TS wants 90 kHz units: `pts = (us - base) * 9 / 100`. Rebase to
   zero on the first frame because the camera's clock starts wherever it likes.
2. **One Baichuan connection per stream**, not per camera. Main and sub are independent
   connections (production already runs `nlcat` against 192.0.2.11 for both at once),
   but a second connection to the *same* stream gets `End of Stream`. So the supervisor's
   unit of work is a stream, not a camera: never serialise main and sub behind one lock.
   A stray connection blocks everything on that stream, including its own replacement.
3. **Close the stream explicitly on shutdown.** Sending the video-stop message is what
   releases the camera's session. A process killed without doing so leaves the camera
   refusing connections for minutes. Handle SIGTERM/SIGINT and close every camera before
   exiting.

Note the inversion from `neolink-cat`: there, `--reconnect` had to stay **off**, because
go2rtc supervised the process and a retrying process squatted the camera's one connection
and blocked its own replacement. Here reostream *is* the supervisor and is the only
thing holding that connection, so retrying inside the daemon is correct.

### Streams: there are three, not two

The firmware carries the literal list `mainStream,subStream,externStream`. `externStream`
is the third, "balanced" stream (the protocol's `stream_id 04`), and it is not exposed
in the Reolink UI. The firmware also has `EXTERNSTREAM_720P_SET` / `_RESET` and
`MSG_ENC_EXTERNSTREAM_720P_SET`, so its resolution is settable; observed at 896x512.

reostream treats all three as first-class from Phase A: `streams = ["main", "sub", "extern"]`
per camera, served as `/<cam>.ts`, `/<cam>_sub.ts` and `/<cam>_extern.ts`. This costs
almost nothing (a stream is just another supervised connection), and a balanced stream
observed at 896x512 is a genuinely useful middle option between a 4K main stream and a
small substream, particularly for the cam wall and for remote viewing over the measured
~20 Mbps uplink.

Configuring `externStream` (via `MSG_ENC_EXTERNSTREAM_720P_SET`) is Phase C work alongside
`GopCfg`; consuming it is Phase A.

### Keepalive

The camera times out sessions it stops hearing from (its firmware logs
`session:%u login timeout`), and the official NVR heartbeats continuously. `neolink-cat`
pings every 10s and that has proven sufficient; reostream starts there and moves to a
real `HeartBeat` in Phase B.

**Do not put the keepalive in a `select` alongside the frame channel.** Frames arrive
every ~40ms, so the frame case is almost always ready and a timer case beside it is
effectively never chosen. Check elapsed time explicitly after each frame.

## Fan-out

The one piece of shared state, and the design's most important boundary: **the camera
never waits on a client.**

Each camera holds a broadcast hub. The camera goroutine writes TS chunks to it and never
blocks. Each connected client has its own buffered channel; if a client's buffer fills,
that client is **disconnected**, not buffered and not waited on. No client can slow or
wedge a camera: that is precisely the failure mode that killed Neolink.

A new client gets:

1. The cached PAT/PMT immediately, so its decoder can start.
2. Then packets from the next keyframe onward. Starting mid-GOP only makes a decoder
   complain about references it never saw.

**Join latency therefore equals the GOP length**, which is why `GopCfg` (Phase C) matters:
it turns join time into something we tune rather than inherit.

## HTTP endpoints

| Endpoint | Returns |
|---|---|
| `GET /<cam>.ts` | MPEG-TS, main stream. The primary product. |
| `GET /<cam>_sub.ts` | MPEG-TS, substream. |
| `GET /<cam>_extern.ts` | MPEG-TS, extern (balanced) stream. |
| `GET /<cam>.jpg` | Most recent keyframe as a still. Cheap, and a health check that does not open a stream. |
| `GET /api/status` | JSON with per-camera fields: connected, fps, bitrate, last-keyframe age, client count, reconnect count, ping failures, camera-reported `PerformanceInfo`. |
| `GET /metrics` | The same, Prometheus format. |

Served on one port (default 8560). Content type `video/mp2t`, no buffering, flush per
chunk.

## Audio

`neolink-cat` drops audio, which is why the `record`+`audio` roles in the current Frigate
config are nominal. reostream muxes audio onto its own PID with its own PTS.

Query `AudioEncodeAbility`/`audioCfg` at connect time so the codec is known rather than
guessed. AAC passes through into the TS. ADPCM cameras are **reported in `/api/status`
and their audio dropped**, not silently transcoded: transcoding would put ffmpeg back
in the path, which is a non-goal.

## Configuration

TOML. One file, one block per camera:

    listen = "0.0.0.0:8560"

    [[camera]]
    name = "pano"
    address = "192.0.2.11"
    username = "admin"
    password = "..."
    streams = ["main", "sub"]

`password` accepts an environment-variable reference so secrets stay out of the file.
Unknown keys are an error at load, not a warning: a typo in a camera name should fail
loudly at boot rather than serve 404s at 3am.

## Error handling

- A camera failure is isolated to its goroutine. Other cameras are unaffected.
- Connection errors, login failures and stream errors are all handled the same way:
  close cleanly, back off, retry. The daemon never exits because a camera is unhappy.
- A failed ping means the session is already gone: tear down and reconnect rather than
  stream into a void.
- Client write errors drop that client only.
- Config errors are fatal at startup and only at startup.

## Testing

- **`ts` package:** unit tests on synthetic frames: PTS monotonicity, the 90 kHz
  conversion, `microseconds` wraparound, continuity counters, PAT/PMT cadence, H.264 and
  H.265 stream types.
- **`baichuan` package:** table-driven tests against **recorded captures**. Decoding a
  real login exchange and real media packets from a fixture is what makes a from-scratch
  protocol implementation trustworthy. Note there is **no existing capture**:
  `~/cap28.pcapng` is unrelated traffic (a payment gateway capture) and must not be used.
  Capturing fixtures is the first task of the protocol plan.
- **Fan-out:** a fake frame source. Assert a new client joins on a keyframe, and that a
  client which never reads is disconnected rather than growing memory without bound.
- **End-to-end:** point at one real camera for 120s and assert with `ffprobe`: frame
  count against wall time (the `neolink-cat` baseline is 3011 frames in 120.68s = 24.95fps
  exactly), zero decode errors, flat RSS.
- **Soak:** all cameras on the bench for 24h with `/metrics` scraped, before any camera
  moves off `neolink-cat`.

## Phases

**Phase 0: protocol spike (bounded, days).** Connect, log in, and pull decodable frames
from one camera in Go. This is the project's only real risk: the login handshake and its
crypto are where a subtle mistake produces "works for a day, fails at 3am". Success is a
Go program that writes an elementary stream `ffprobe` accepts. **If this stalls, fall
back to Rust with `neolink_core` and accept AGPL**: a few days lost, not the project.

**Phase A: the daemon.** Everything above: config, camera supervisor, TS muxer, HTTP
fan-out, audio, status, metrics, snapshots. Deployable and soak-tested on the bench while
`neolink-cat` keeps serving production.

**Phase B: `HeartBeat`.** The NVR heartbeats continuously and the camera logs session
timeouts. Capture it, implement it, keep the proven 10s ping as fallback so this cannot
regress.

**Phase C: `GopCfg`.** Read and set the keyframe interval. Turns client join latency
into a tunable. Ranked above stitching because it affects every camera and every viewer.

**Phase D: `BinoStitch` and offset adjust.** For the Duo 3 pano, currently guessed at
through the HTTP API. The firmware exposes more than a stored config:

    BinoStitch                            parameter, with x2s/s2x serialisers
    GET_BINO_STITCH_CFG / SET_BINO_STITCH_CFG
    MSG_CFG_STITCH_GET / MSG_CFG_STITCH_SET
    MSG_NETC_BINO_OFFSET_ADJUST / MSG_NETC_BINO_OFFSET_GET
    MSG_SNAP_BINO_ADJUST_RESULT_GET / MSG_SNAP_BINO_SCREEN_ADJUST
    CamCenterDiff / CamWidthDiff / CamHeightDiff / CamStitchPara / CamEcsPara

So there is a live offset-adjust path and a snapshot-based readback of the result: the
camera can be told to re-align its lenses and report how it went.

**Phase E: `fishEyeSubChnCtrl` / `fishEyeCfg`.** The 360 camera has view options we
have never been able to reach. The firmware exposes:

    fishEyeCfg / GET_FISH_EYE_CFG / SET_FISH_EYE_CFG
    MSG_CFG_FISH_EYE_GET / MSG_CFG_FISH_EYE_SET
    fishEyeSubChnCtrl / FISH_EYE_SUBCHN_CTRL_V20 / MSG_CFG_FISH_EYE_SUBCHN_CTRL
    net_fish_eye_cfg_x2s / net_fish_eye_cfg_s2x / nets_param_fish_eye_subchn_x2s

So there is both a fisheye configuration (mount orientation and view mode) and a
*sub-channel* control: meaning the camera can very likely emit dewarped views as
additional channels rather than us cropping them out afterwards. The current Frigate
config carries commented-out ffmpeg `crop`/`vflip` filters doing exactly that job by hand.
If the camera can do it, that is GPU transcoding deleted from the path rather than moved,
and it is also how the 360's "different view options" become available at all.

The field names inside `fishEyeCfg` are not readable from the firmware strings (the only
`ViewMode` in the binaries belongs to ONVIF, not Baichuan), so this phase starts by
capturing a `GET_FISH_EYE_CFG` exchange from the Reolink app.

## What we know is missing from Neolink

Extracted from the NVR firmware's parameter table (144 names) and diffed against
`neolink_core`: **106 are absent.** Most are irrelevant here: cloud, battery, FTP,
doorbell. The relevant ones are the phases above, plus these, deliberately deferred:

- `DayRecords`, `FileInfoList`, `ReplaySeek`, `CoverPreview`: playback. Nobody has ever
  implemented it. Not needed while Frigate records.
- `AiCfg`, `AiDetectCfg`, `AlarmArea`, `trackLimit`: detection config.
- `OsdChannelName`, `OsdDatetime`: the burned-in clock, useful for latency measurement.
- `Compression`, `RecEncCfg`: bitrate/resolution/codec control.
- `NetworkDiagnosisData`: the camera's own view of its link, worth having given the
  switch-uplink flapping history. Folded into `/api/status` if cheap.

**Caveat:** these are names and serializer symbols from firmware. Wire IDs and XML bodies
must still be captured. The parameter structs carry a tag word that is *not* the Baichuan
message ID: verified against `TalkAbility`=10, `PtzControl`=18, `Snap`=109,
`AbilityInfo`=151, none of which match. The firmware says what to look for; the capture is
what we implement from.

## Deployment and cutover

Parallel build, hard cutover per camera. reostream runs on the bench against one camera
while the other cameras stay on `neolink-cat`. Cameras move one at a time, each after its
own soak. Rollback for any camera is the existing `nlcat.sh` source in the Frigate config.

Because a camera allows one Baichuan connection, a camera being tested on reostream must
be removed from the `neolink-cat` path first, not run in parallel with it.

## Success criteria

1. `ffprobe` reports the same frame-count-to-wall-time ratio as `neolink-cat` (24.95fps),
   with zero decode errors over 120s.
2. RSS flat across a 24h soak with all cameras connected.
3. Frigate runs detect and record from `http://…/<cam>.ts` with `skip=0.0`, at or below
   the current 143% CPU, with `neolink-cat` and the per-camera `ffmpeg -c copy` gone.
   go2rtc remains only for browser live view.
4. A client that stops reading is disconnected; camera fps is unaffected.
5. Killing and restarting reostream reconnects every camera without the
   connection-refused window that a hard kill causes.
6. The Mac plays `http://…/pano.ts` cleanly. If it does, that settles the open question
   from the notes: the HEVC degradation was go2rtc's H.265 RTP packetisation, not the
   stream: a real answer rather than a transcode routing around it.

## Licence

My own copyright, licence to be chosen at first publish. Since no Neolink code is
used, AGPL is not inherited and the choice is free. Credit thirtythreeforty and
QuantumEntangledAndy in the README for the prior art that made the protocol legible.

## Prerequisites

Go is not installed on the development host (`golang-go` candidate is 2:1.22). Install a current
toolchain before Phase 0.
