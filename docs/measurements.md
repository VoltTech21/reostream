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

## The balanced stream, measured 2026-09-08

Reolink cameras carry a third stream the app does not expose. The firmware calls it
`externStream` and has `EXTERNSTREAM_720P_SET`, implying its resolution is settable.
Neolink never implemented it, so on this fleet it had never been reachable.

Measured on four cameras, all identical:

| stream | resolution | codec | rate | bitrate |
|---|---|---|---|---|
| main | 3840x2160 | HEVC | 25 fps | 6.4 Mbps |
| extern | 896x512 | H.264 | 20 fps | 1.0 to 1.7 Mbps |
| sub | 640x360 | H.264 | 10 fps | 0.29 Mbps |

Zero decode errors over 10 second captures, and it carries AAC audio like the others.

It needed no enabling message: connecting with the extern stream id was enough. An earlier
probe reported zero frames, which was a tool buffering its output and losing it when killed,
not the camera refusing.

What it is good for is the gap it fills. Main to sub is a factor of 22 in bitrate with
nothing between, so a viewer either takes a 4K stream or a thumbnail. This sits in the
middle and, being H.264 rather than HEVC, avoids both the browser codec problem and the
tiled HEVC hardware decode failures seen on one machine here.

It does not replace a full resolution H.264 transcode for browser live view, because it is
896x512. It is a cheaper tier alongside that, not a substitute for it.

## First production cutover, 2026-09-09

One motion-only camera moved from the previous tool to reostream, with the recorder
consuming `http://host:8560/<name>.ts` directly.

It failed on the first attempt and that failure was the whole value of doing it.

### What broke

The recorder's restreamer rejected every stream with `mpegts: wrong adaptation size`, its
watchdog reported no valid recording segments for 120 seconds, and the cutover was rolled
back. Client churn was the visible symptom: 107 dropped consumers in two minutes while the
camera side sat at a healthy 25 fps.

The cause was PCR delivery. The muxer emitted PCR on its own adaptation-only packet, which
the MPEG-TS spec requires to carry an adaptation_field_length of exactly 183. That is valid,
and ffmpeg accepts it. The restreamer's demuxer rejects any adaptation field longer than 182
and therefore cannot parse an adaptation-only packet at all. Measured on a 20 second capture
of a 4K HEVC main stream: 84,680 packets, of which 239 were adaptation-only and every one
was rejected.

### Why nothing caught it earlier

Every check that passed was made against ffmpeg:

- the real capture test, which runs ffprobe
- a structural validator written the same day, whose rules did not include this one
- a deliberate design review, which approved the packet as legitimate MPEG-TS, correctly
- 8.5 hours of soak, whose consumer was ffmpeg

Valid per specification and readable by the consumer you actually have are different tests,
and only the cutover ran the second one.

The origin was a defective test helper. The plan specified the PCR in the adaptation field of
a packet that also carried payload; the plan's own helper for finding a PES header assumed
payload began at a fixed offset, which contradicted that. The contradiction was resolved by
moving the PCR to its own packet, and the interop failure followed from there.

### After the fix

PCR now rides in the adaptation field of a packet that also carries payload.

| | before | after |
|---|---|---|
| packets the restreamer rejects | 239 per 20s | 0 |
| dropped consumers | 107 in 2 minutes | 0 |
| consumers attached | 2 to 8, churning | 1, stable |
| recording segments | none for 120s | continuous |

Recorded segments verified against segments the previous tool wrote for the same camera in
the same hour: identical, 250 frames in 10.068 seconds, valid mdat, audio present, and two
decoder messages per segment on both, so that count is inherent to the camera rather than
anything the bridge introduced.

## Full fleet cutover, 2026-09-09

All eight cameras and ten streams moved at once, with the recorder's own sources removed
from its config and the container restarted so the change survives a restart. Six cameras
came up immediately. The fisheye and the pano, main and sub, did not.

### Two cameras, four streams, zero frames

Both reported `no video frame in 30s: camera is answering but not sending media (held
session)`. That message is the media watchdog doing its job, but the diagnosis it offers is
wrong here: nothing was holding a session. The camera had authenticated, was answering
pings, and was sending roughly 6 Mbps that never became a frame.

Ruling things out, in the order that turned out to be cheapest:

| test | result |
|---|---|
| literal password in place of `$CAM_PW` | still zero frames, so not the environment |
| the same binary run on the host, one camera, nothing else | reproduced, so not the container |
| a deliberately wrong password on a working camera | `empty login reply`, so authentication was genuinely fine |
| two clients on the same camera at once | both healthy, so not a connection limit |
| dumping message ids and extension XML | the fault |

The last one took a minute and should have been first. The two cameras put an extension
header on every message of a media packet, not just the first, so every message looked like
a packet boundary and each one discarded the partial frame before it. A 940 KB keyframe
spanning hundreds of messages never completed.

### The second fault, which the first was hiding

With framing fixed both cameras streamed, and the fisheye then produced 929 decoder warning
lines per 20 seconds against 3 on a healthy camera. Every one was in the last twelve percent
of the picture: `error while decoding MB x 141..159` on a 160 row frame.

A packet's size field counts the coded picture only, not the metadata that precedes the
first NAL start code, so a packet is `hdr + prefix + size` bytes. Consuming `hdr + size`
dropped the tail of every frame. Trimming the prefix on the way out, which this had been
doing since the HEVC work, hides half the fault: the decoder gets a clean start code and
only the bottom rows go missing.

A first fix capped the prefix search at 256 bytes, which covered the fisheye's 104 to 184
and cut every longer frame on the pano's substream, where the prefix runs 328 to 352. That
one stream stayed at 438 warnings while the other nine dropped into the noise.

### Result

Decoder warning lines per 20 seconds, measured through the recorder's own ffmpeg against
each stream, after both fixes:

| stream | before | after |
|---|---|---|
| fisheye | 929 | 1 |
| fisheye sub | not streaming | 0 |
| pano | not streaming | 1 |
| pano sub | 438 | 8 |
| the six single lens cameras | 3 to 10 | 0 to 5 |

What is left is join noise: a client attaching mid GOP. Ten of ten streams, zero restarts,
zero dropped consumers, and all eight recorder cameras at their configured 5 fps with no
skipped frames.

## Every stream, 2026-09-09

Extended from ten streams to twenty-three: main, sub and balanced on all eight cameras,
except the fisheye, which does not serve the balanced stream. The recorder now has no
direct camera connection of any kind; its detect substreams, which had stayed on RTSP
through the first cutover, moved at the same time.

| | value |
|---|---|
| streams | 23 of 23, zero restarts |
| aggregate | 75.2 Mbps |
| reostream CPU | 53% of one core |
| reostream RSS | 37 MB |
| recorder cameras | 8 of 8 at their configured 5 fps, zero skipped |

Cost per stream is roughly flat: ten streams at 60 Mbps took 48% of a core and 28 MB, and
twenty-three at 75 Mbps take 53% and 37 MB. The work is per byte, not per connection.

The balanced stream is exposed as a third live-view tier rather than as a replacement for
the full resolution H.264 transcode. At 896x512 it is not a substitute for that; it is a
cheaper option beside it, and it costs no GPU at all because the camera encodes it.

One thing this makes visible: reostream opens every configured stream at startup and holds
it whether or not anything is consuming it. For a main stream feeding a recorder that is
correct. For a balanced stream nobody is watching it is about 13 Mbps of pointless traffic
across seven cameras. An on-demand mode is worth having and does not exist.
