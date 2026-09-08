# Where reostream is, and what's next

Updated 2026-09-08 (weekend inspection).

## State

Phase 0 is done. `internal/baichuan` is a complete, from-scratch Baichuan client in Go,
tested hermetically against committed H.264 and H.265 captures, and `cmd/bcprobe` streams
real cameras. Measured on 192.0.2.18:

| Stream | Result |
|---|---|
| sub | h264 640x360, 199 frames / 20s, 0 decode errors |
| main | hevc 3840x2160, 497 frames / 20s, 1 decode error |
| extern | h264 896x512 (the stream the Reolink UI does not expose) |

Production is untouched and still runs `neolink-cat` on all 8 cameras. It survived
three and a half days unattended: 64/64 sampled weekend segments decoded clean, all 8
cameras detecting every day, max 1.8 min recording gap. Two defects in the *wrapper*
(not the protocol code) came out of that inspection and both feed directly into the
design below: see [[neolink_v2]] for the full write-up.

Read `docs/protocol.md` before touching the protocol; it records what the wire actually
does, which contradicts the public docs in several places.

## Next: the daemon (Plan 2)

Ordered so the open question gets answered early rather than last.

1. **Port `ts.rs` to Go**: the MPEG-TS muxer, emitting real PTS from the camera's
   `microseconds`. Our own code from `neolink-cat`, so it ports freely.
2. **HTTP fan-out**: always-on connection per stream, broadcast to clients, new client
   joins at the next keyframe, a client that falls behind is disconnected rather than
   buffered.
3. **Config and supervisor**: one goroutine per *stream* (not per camera), reconnect
   with backoff, clean close on SIGTERM.

**What the weekend proved about this design.** The production leak is that `nlcat.sh`
runs `neolink-cat | exec ffmpeg`, so killing the shell orphans both children to PID 1
where they hang forever holding the stream's one Baichuan connection: 16 stranded pairs
accumulated over 3.5 days, and an orphan blocks its own replacement with `End of Stream`.
A single daemon owning goroutines cannot have this failure mode at all, which is the
strongest argument yet for reostream over the wrapper approach. Two requirements fall
out of it:
- The supervisor must **fully close a stream before reconnecting it** (send the stream-stop
  message, confirm teardown), never overlap old and new connections to the same stream.
- SIGTERM must drain every stream's close handshake before exit, with a timeout, because
  an abrupt exit leaves the camera holding sessions for minutes.

4. **Audio in the muxer, not after it.** `neolink-cat` dropped `Aac`/`Adpcm` as "a video
   pipe" and production lost audio silently for days: nothing alerts on a missing track.
   The Go muxer should carry a second PES stream and its PMT entry from the start rather
   than bolt it on, and the soak test should assert the audio track exists.

Steps 1 and 2 alone produce something mpv can play, which is enough to settle the open
question below. Status, metrics, snapshots and audio come after.

Then: bench soak with all cameras → per-camera cutover → protocol phases B–E
(`HeartBeat`, `GopCfg`, `BinoStitch`, fisheye), per the design doc.

## Open items

- **Does HTTP-TS fix the remote HEVC degradation?** The unresolved question from the
  Neolink notes: the pano decoded cleanly server-side but showed 9–27 errors/15s on the
  Mac over RTSP, suspected to be go2rtc's H.265 RTP packetisation (`pkg/h265/rtp.go`).
  Serving HTTP-TS tests it directly. If the Mac plays `pano.ts` cleanly, that is the
  answer, and the `pano_wall` transcode can go away. **Test this as soon as step 2 works.**
- **What is inside the HEVC frame prefix?** 80, 112 or 152 bytes before the first NAL
  start code, currently trimmed and discarded by `Frame.Video()`. Worth ~20 minutes: if it
  carries frame metadata it may feed the `GopCfg` work, and it is unknown territory that
  no other client appears to use.
- **One HEVC decode error remains** in a 20s capture, down from 13. Not yet chased.
  Possibly the first frame of the stream; possibly real.
- **Can the cam wall pull `externStream` directly?** The wall currently consumes a
  transcoded 1920x544 pano tile from go2rtc (`pano_wall`). If 896x512 extern is good
  enough for a tile, that transcode disappears too. Test once HTTP serving exists.
- **Licence undecided.** No Neolink code is used, so AGPL is not inherited and the choice
  is open. Decide before first publish.

## Pano stitching: what the NVR firmware settled (2026-09-08)

**The NVR does not stitch and never sees an unstitched stream.** Confirmed by string
analysis of `netserver`, `netclient`, `rtsp`, `recorder` and the four `.so` files: zero
warp / remap / blend / seam / dewarp / homography code anywhere, and no knowledge of
`7680` or any dual-pano resolution. Every bino/stitch symbol in it is a *message name*.
It asks the camera what it supports (`sc_get_support_ipc_bino`, `g_ipc_bino_type`) and
sets the mode. The camera's SoC stitches before encoding. There is no stitching engine
in the NVR to learn from.

**But the firmware gives away the model, which is much simpler than assumed.** The stitch
is a 2D translational offset plus a blend width, not a homography or mesh warp:
`get bino camera width difference` / `height difference`, `netc_msg_bino_linewidth_get`.
Two parameter blobs: `CamStitchPara` (geometry) and `CamEcsPara`, the latter next to
`support_isp_same_brightness`: the two sensors' ISPs are already brightness-matched, so
the seam has no exposure step. Auto-alignment is snapshot-driven
(`MSG_SNAP_BINO_SCREEN_ADJUST`, `MSG_SNAP_BINO_ADJUST_RESULT_GET`), with a factory
`optocoupler calib` for the physical mount.

Message family to implement: `MSG_CFG_STITCH_GET/SET`, `MSG_NETC_BINO_OFFSET_GET/ADJUST`,
`MSG_SNAP_BINO_ADJUST_RESULT_GET`, `MSG_SNAP_BINO_SCREEN_ADJUST`. Per the standing
provenance rule, the firmware says only WHAT exists; confirm each on the wire and
implement from the capture.

Scope this in three tiers, deliberately kept apart:

1. **Stitch parameter control (core, phase D).** Read/write the offsets, linewidth and
   ECS params over Baichuan. Cheap protocol work, parity with the app, something Neolink
   never had. Lets the seam be corrected without Reolink's app.
2. **SPS `general_level_idc` rewrite (opt-in flag, bitstream only).** Dual pano stamps
   level 150 (L5.0, max 8,912,896 px) on a 16,588,800 px picture, which is why the Arc
   A310 refuses it (`Failed to end picture decode issue: 23`). Rewriting it to 183 (L6.1)
   in-flight is one byte, no decode, no re-encode. The A310's media engine handles 8K
   HEVC, so this is plausibly the whole fix: untested, and worth testing because it also
   answers whether tiled ultra-wide HEVC decodes at all, which is the question the
   `pano_wall` transcode currently exists to avoid.
3. **Compositing the halves ourselves (optional module, OFF by default, outside the
   serving path).** Decode → offset composite → blend → re-encode contradicts the
   "no transcode in the serving path" non-goal that exists because every Neolink bug lived
   in its media layer. If built, it must be a separate opt-in output that the core
   supervisor never depends on.

**Do not expect tier 3 to improve quality.** Dual pano is 16.6 MP at ~10.9 Mbps = 0.033
bits/pixel against single pano's 4.7 MP at 10.2 Mbps = 0.108, and the camera's bitrate
ladder tops out at 12288 kbps in both modes. Stitching ourselves skips the downscale but
cannot add bits, and a pure translation cannot fix parallax, so close objects crossing the
seam still ghost. Single pano remains the right production setting for a parking lot.
Production is currently 4096x1152, verified 2026-09-08; the camera has silently reverted
to 7680x2160 before, so check it when pano behaves oddly.
