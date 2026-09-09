# Where reostream is, and what is next

Updated 2026-09-09.

## State

The daemon is built and measured against a live 8 camera fleet. It runs from a TOML config,
supervises one connection per stream with backoff, carries audio, starts new subscribers at a
keyframe, and reports health over `/api/status` and `/metrics`. See README for the measured
numbers and `docs/measurements.md` for how they were taken.

Production is fully cut over. All eight cameras and ten streams run through this daemon,
the previous tool is gone from the recorder config, and no wrapper processes remain.

## What was found by pointing it at real cameras

Worth reading before adding features, because every one of these passed the unit tests:

- **No idle read timeout.** A camera that completed login then went silent hung the client
  forever. That is the client side signature of a held session.
- **`Supervisor.Run` was not idempotent.** A second call duplicated every stream goroutine.
- **Audio and video shared one clock.** 2,265 timestamp discontinuities per minute on a live
  HEVC stream. The cameras send no timestamp at all in audio packets, and once the video
  clock passes 2^31 microseconds of camera uptime every audio frame reads as a backward jump.
  The committed capture was taken at 28 minutes uptime, just under the threshold, which is
  why no fixture reproduced it.
- **Connected but silent streams never restarted.** The idle timeout watches bytes, and a
  camera holding a stale session still answers keepalives, so reads succeed while no video
  arrives. Fixed with a media watchdog distinct from the byte one.
- **An extension header is not a packet boundary.** The rule was "a packet begins at a
  message carrying an extension header". Six cameras agree, because they send one only on
  the first message of a packet. The fisheye and the pano put an extension carrying
  `checkPos` and `checkValue` on every continuation message, so every message looked like a
  new packet and a 940 KB keyframe spanning hundreds of messages never completed. Both
  cameras logged in, answered pings and moved 6 Mbps while delivering zero frames. The
  boundary is `binaryData`.
- **A packet is longer than its size field says.** Size counts the coded picture, not the
  metadata before the first NAL start code, so a packet is hdr+prefix+size bytes. Trimming
  the prefix on the way out looked like a fix and was not: the tail of every frame was
  still being dropped as filler, which cost the fisheye the bottom eighth of every picture
  and left the pano's HEVC with a residual decode error the earlier measurement recorded
  and did not chase.

Two documented assumptions also turned out to be wrong:

- The remote HEVC degradation was never the restreamer's H.265 packetisation. Same root cause
  as the local degradation, the camera's own RTSP server, already fixed by moving to Baichuan.
- "One connection per stream" is really one connection per **main** stream. Sub streams accept
  at least two simultaneous clients, both healthy, and the incumbent is never harmed by a
  contender.

## Next

1. **Long soak on the full fleet.** The single camera soak ran 8h34m clean. Everything
   measured since the cutover is minutes to hours across ten streams.
2. **Battery cameras.** Never tested, and the biggest gap for anyone else: battery models are
   why most people used the previous tool, since they have no RTSP at all, and they sleep,
   wake on motion and send battery state messages this client has never seen.
3. **Protocol phases.** Phase B is implemented and declined by every camera here: the
   heartbeat is message 5, recovered from firmware, and all three models answer 421. The
   proven ping stays, which is what the design said to do. Phase C should be dropped
   rather than built: no camera here reports `supportGop`, so there is nothing to test it
   against. Phases D and E are done over the HTTP API, because their message ids are still
   unknown.

   Old text follows. `HeartBeat`, then `GopCfg`, then the fisheye and stitching work in
   `docs/phase-d-e-groundwork.md`. That document has the field names already; what it lacks
   is the numeric message ids, which one capture of an NVR talking to a camera would give.

## Two-way audio

Implemented and confirmed audible on 2026-09-09: `TalkAbility` to ask what a camera
accepts, `TalkConfig` to open a session, `Talk` to carry IMA ADPCM. Seven of eight cameras
negotiate; the eighth answers 422 after a session has been opened on it.

It is worth knowing how much of this looked finished while being broken, because the same
shape will recur across the rest of the camera surface:

- The first version reported success when the camera had refused the config outright.
  Nothing checked the reply. Checking it turned a working feature into a status 400.
- Status is a 16 bit little endian field. Read a byte at a time it says 144, which is not a
  code at all, and sends you looking at camera state instead of at a malformed message.
- The last bug was accepted by the camera without any error and simply produced no sound.
  Nothing on the wire distinguished it from success.

That last one is the general problem with this whole area: an actuator has no reply that
proves it acted. The way out is to measure the effect rather than the call, which for audio
meant a bandpass filter on the camera's own stream and on its neighbours' (see
`docs/protocol.md`). Anything else implemented here needs an equivalent, and the ones that
have no measurable effect should be treated as unverified no matter how clean the code is.

## Wanted, not started

- **A read only status page.** `/api/status` and `/metrics` already carry everything; this is
  rendering them. The question an operator actually asks is whether a camera is streaming
  right now, and today that means reading JSON by hand. Cheap, no dependencies, fits the
  project.
- **A camera control surface**, separate from this daemon. `docs/camera-capabilities.md`
  records what the cameras expose and it is a lot: full ISP control, IR and LED, on camera AI
  with per type sensitivity, webhooks, a floodlight on one camera, fisheye view modes and
  pano stitching. None of it is used today. It does not belong inside reostream, whose value
  is a small surface and a short list of non goals, but it is worth having.

## Open items

- `cmd/reostream` is at 15% coverage. The shutdown ordering is tested; flag parsing is not.
- `MaxMessageSize` is pinned to today's maximum camera resolution. It rejects an oversize
  header loudly rather than corrupting, so a future higher resolution camera fails clearly at
  dial time rather than silently.
- No published container image yet, though the Dockerfile exists.
