# Measurements

These are measurements taken on the author's own cameras. Anyone reproducing them should
expect their own numbers to differ by camera model and firmware.

## RTSP vs Baichuan, Duo 3 PoE

Camera: Reolink Duo 3 PoE, firmware v3.0.0.5049_2506302188, 4096x1152 HEVC.

Method: both transports captured simultaneously for 30 minutes, then compared at
identical wall-clock seconds. This comparison is exact rather than approximate because
the camera burns its clock into the image, so a given second on one transport can be
matched pixel-for-pixel to the same second on the other.

| transport | segments | segments with bitstream errors | worst corruption score |
|---|---|---|---|
| Baichuan | 184 | 0 | 1.79 |
| RTSP | 178 | 147 (83%) | 122.98 |

Same encoder, same camera, same second, so the only variable is the delivery path.

The RTSP defects were all slices of one picture disagreeing with each other:

- `Ignoring POC change between slices`
- `Non-matching NAL types of the VCL NALUs`
- `Could not find ref with POC N`

RTSP also reported a ragged 24.75 fps (99/4) where Baichuan reported exactly 20/1, which
is what the camera is configured for.

## A method trap: zero decode errors is not clean

Zero ffmpeg decode errors does not mean clean video. A full hour of 360 segments scanned
as zero errors while being visibly corrupt, because the RTSP path emits syntactically
valid HEVC that decodes to the wrong picture. There is no substitute for rendering frames
and looking at them.

Whole-frame metrics do not separate corrupt from clean either. Scene score and saturation
were both tried and both failed, because the garbage from the defects above is too small a
fraction of a 4096x1152 frame to move a whole-frame statistic.

What did work was a difference blend across a cropped strip of the frame, used as a
ranker to sort candidates for eyeballing rather than as an automatic detector.

## Fleet comparison, fisheye camera

25 second samples, Baichuan vs the camera's own RTSP:

| transport | fps |
|---|---|
| Baichuan | 24.6 |
| RTSP | 19.3 |

That is about a 21% frame loss over RTSP, on a camera whose RTSP metadata claims 40/1.
Both Baichuan feeds measured 0 bitstream errors in the sample; healthy RTSP cameras in
the same fleet showed 1 to 3 per 25 seconds.

## Three streams per camera

Cameras carry three streams, not the two the Reolink app exposes. Measured on one camera:

| stream | codec | resolution |
|---|---|---|
| sub | H.264 | 640x360 |
| main | HEVC | 3840x2160 |
| extern (undocumented) | H.264 | 896x512 |

Frame counts from the Go client, 20 second captures: sub delivered 199 frames with 0
decode errors, main delivered 497 frames with 1.

## Neolink, as of September 2026

Neolink is the prior art this project learned the protocol from. As of this writing: last
commit 2025-01-30, 124 open issues, and a memory leak reported since 2022.

## reostream serving MPEG-TS over HTTP, 2026-09-08

First end to end measurements of the daemon against live cameras. Six cameras, all on
their substreams, because every main stream on this fleet was in use at the time.

One camera measured through the full path, camera to Baichuan to muxer to HTTP to ffmpeg:

| run | frames | duration | fps | stream rate | RSS |
|---|---|---|---|---|---|
| 60s | 591 | 59.158s | 9.99 | 10 | 13.4 MB |
| 30s | 292 | 29.178s | 10.01 | 10 | 13.4 MB |

Frame count matching wall clock is the result that matters. An elementary stream carries
no timing of its own, so a muxer that invents timestamps produces a frame count wildly out
of step with real time. Getting this wrong previously produced a 216 fps stream from a
20 fps camera and a player dropping 10,387 frames.

Resident memory stayed flat with a client attached, which is the other thing being watched:
a climbing figure would mean the fan-out was buffering for a slow reader instead of
dropping it.

Five further cameras were checked with the protocol probe alone, 6 second captures, and all
five returned an identical 59 frames with 6 keyframes, H.264, and no resynchronisation.

### Decode errors at connect, and why

A capture started mid stream reports a small number of `non-existing PPS`,
`decode_slice_header error` and `no frame` messages, roughly 9 groups in 60 seconds, and
then nothing. The saved file re-reads with zero errors.

The cause is that a client currently joins wherever the stream happens to be rather than at
a keyframe, so its decoder sees slices before the parameter sets that describe them and
complains until the next keyframe arrives. It is bounded to connect time and costs nothing
after that, but it is a real gap: keyframe aligned join is specified in the design and is
not yet implemented.

### Not yet measured

No live HEVC stream has been tested. Every live measurement above is H.264 substream. HEVC
is covered only by the committed capture in the test suite. Every HEVC main stream on this
fleet was held by the tool being replaced, and a camera permits only one connection per
stream, so there was no free HEVC stream to measure. This is worth closing during the
per camera migration, when a camera moves across permanently and the measurement costs
nothing.

## The remote HEVC degradation question, answered 2026-09-08

An open question carried for weeks: the panoramic camera decoded cleanly on the server but
showed 9 to 27 bitstream errors per 15 seconds when played on a laptop across the network.
The leading theory was that the restreamer's H.265 RTP packetisation corrupted it in transit,
which would have been an argument for serving MPEG-TS over HTTP instead.

Measured again from the same laptop, same camera, same transport, after the camera had been
migrated from its own RTSP server to the Baichuan path:

| sample | window | bitstream errors |
|---|---|---|
| 1 | 15s | 0 |
| 2 | 15s | 0 |
| 3 | 15s | 0 |
| 4 | 15s | 0 |
| longer run | 30s | 0, 584 frames, 20.03 fps against a configured 20 |

**The theory was wrong.** The restreamer's packetisation was never the cause. The remote
degradation had the same origin as the local degradation, the camera's own RTSP server, and
migrating the source to Baichuan fixed both at once. There was never a second problem.

Worth recording because a negative result here is as useful as a positive one: it removes a
reason to prefer HTTP-TS that turned out not to exist. The reasons that remain, no session
pool, no per-consumer queues, no orphaned connections holding a camera's session, are all
about the daemon's own failure modes rather than about the wire format.
