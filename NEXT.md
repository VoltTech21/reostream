# Where reostream is, and what is next

Updated 2026-09-08.

## State

The daemon is built and measured against a live 8 camera fleet. It runs from a TOML config,
supervises one connection per stream with backoff, carries audio, starts new subscribers at a
keyframe, and reports health over `/api/status` and `/metrics`. See README for the measured
numbers and `docs/measurements.md` for how they were taken.

Production still runs the previous tool on all cameras. Nothing has been cut over.

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

Two documented assumptions also turned out to be wrong:

- The remote HEVC degradation was never the restreamer's H.265 packetisation. Same root cause
  as the local degradation, the camera's own RTSP server, already fixed by moving to Baichuan.
- "One connection per stream" is really one connection per **main** stream. Sub streams accept
  at least two simultaneous clients, both healthy, and the incumbent is never harmed by a
  contender.

## Next

1. **Long soak.** Everything so far was measured in minutes to hours. The 71.6 minute camera
   clock wrap has still never been crossed against a real camera.
2. **Per camera cutover.** One motion only camera first, left overnight, before anything on
   continuous recording moves. Rollback is the previous tool's source line, left commented in
   the recorder config.
3. **Battery cameras.** Never tested, and the biggest gap for anyone else: battery models are
   why most people used the previous tool, since they have no RTSP at all, and they sleep,
   wake on motion and send battery state messages this client has never seen.
4. **Protocol phases.** `HeartBeat`, then `GopCfg`, then the fisheye and stitching work in
   `docs/phase-d-e-groundwork.md`. That document has the field names already; what it lacks
   is the numeric message ids, which one capture of an NVR talking to a camera would give.

## Open items

- `cmd/reostream` is at 15% coverage. The shutdown ordering is tested; flag parsing is not.
- `MaxMessageSize` is pinned to today's maximum camera resolution. It rejects an oversize
  header loudly rather than corrupting, so a future higher resolution camera fails clearly at
  dial time rather than silently.
- No published container image yet, though the Dockerfile exists.
