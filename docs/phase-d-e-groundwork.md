# Groundwork for pano stitching and fisheye control

> **Superseded in part.** This document concluded that the numeric message ids were not
> recoverable from firmware and that only a packet capture would give them. That was
> wrong, and the section below saying so is kept rather than deleted because the reason
> it was wrong is instructive: the search was for parameter strings referenced from a
> table, and the ids live in a dispatch table keyed the other way round, by id, with the
> name inline. 246 ids were recovered statically. `fishEyeCfg` is 443/444,
> `fishEyeSubChnCtrl` is 541, `BinoStitch` is 417/418, and the Baichuan heartbeat is 0,
> not the 5 recorded here. See `docs/control.md`.
>
> The field names below are still good, and still not a specification.

Notes from reading the NVR firmware, 2026-09-08. Nothing here is implemented yet, and
nothing here should be implemented from this document alone: these are names to look for on
the wire, not a specification. The wire is the specification. See CONTRIBUTING for why that
distinction is not negotiable in this project.

The NVR's camera-facing client speaks the same protocol we do, on the same port, so whatever
it does to stitch a panoramic camera or configure a fisheye it does over Baichuan. That makes
it a better reference than capturing the phone app, and it is where these names came from.

## What a camera says it can do

There is an ability table listing the capabilities a camera advertises. Two entries matter
here, sitting alongside familiar ones like `battery`, `motion` and `autoFocus`:

    fishEye
    binoCfg

That is worth using before anything else. Rather than guessing which models support what,
query the ability list and let the camera answer. A fisheye-only feature attempted against a
camera that does not advertise `fishEye` is a wasted capture session.

## Pano stitching

Messages:

    MSG_CFG_STITCH_GET / MSG_CFG_STITCH_SET
    GET_BINO_STITCH_CFG
    MSG_NETC_BINO_OFFSET_ADJUST
    MSG_NETC_BINO_OFFSET_GET
    MSG_SNAP_BINO_ADJUST_RESULT_GET
    MSG_SNAP_BINO_SCREEN_ADJUST

Parameter name: `BinoStitch`. There is also `BinoAdjust` and a `binoType` / `bino_type`
notion, with a log line about a channel's bino type changing from one value to another, so
the camera has modes rather than a single stitching state.

Four separate serialisers exist, which says the feature has four distinct payloads rather
than one config blob:

    net_bino_stitch_cfg_s2x / x2s      the stored configuration
    nets_bino_adjust_x2s               an adjustment request
    nets_bino_adjust_result_x2s        the result of an adjustment
    nets_bino_linewidth_x2s            a line width, presumably the seam

Storage keys the NVR reads and writes, which describe what the configuration consists of:

    CamCenterDiff
    CamWidthDiff
    CamHeightDiff
    CamStitchPara      (a blob, there is a length check against a maximum)
    CamEcsPara         (another blob)

Note these are keys in the camera's parameter store, reached over Baichuan, not XML element
names. The XML element names still have to come from a capture. But knowing the
configuration is centre, width and height differences plus two opaque blobs means a capture
can be read rather than guessed at.

The interesting part is `MSG_NETC_BINO_OFFSET_ADJUST` together with
`MSG_SNAP_BINO_ADJUST_RESULT_GET`. That is not a stored setting being written: it is a
request to re-align the lenses and a snapshot-based readback of how it went. A camera that
can be told to re-align itself and report the outcome is a different proposition from one
whose alignment you nudge by hand through a web UI.

## Fisheye

Messages:

    MSG_CFG_FISH_EYE_GET / MSG_CFG_FISH_EYE_SET
    GET_FISH_EYE_CFG / SET_FISH_EYE_CFG
    MSG_CFG_FISH_EYE_SUBCHN_CTRL
    FISH_EYE_SUBCHN_CTRL_V20

Parameters: `fishEyeCfg` and `fishEyeSubChnCtrl`. Serialisers exist for both, including
`nets_param_fish_eye_subchn_x2s`, so the sub-channel control is its own payload rather than a
field inside the main config.

**Sub-channel control is the part worth chasing.** A dedicated control message for
sub-channels strongly suggests the camera can emit dewarped views as additional channels,
rather than a client cropping regions out of the raw fisheye image afterwards. If that is
what it means, then any setup currently producing dewarped tiles with a crop filter and a GPU
transcode is doing work the camera will do itself.

The field names inside `fishEyeCfg` were unknown when this was first written. They are built
by `net_fish_eye_cfg_s2x` from literals in that function's literal pool. They have since been
recovered; see below.

## The field names, recovered 2026-09-08

These came out of the serialiser functions themselves rather than a capture. The strings are
not visible in a plain dump because position independent ARM code stores an offset in the
literal pool and adds PC to it at runtime, so resolving them means pairing each load with the
add that consumes it. `tools/armstrings.py` does that.

**Still to be confirmed on the wire before anything is implemented.** These are field names
read out of the code that writes them, which makes them a very good guide to what a capture
will contain, not a substitute for the capture.

### fishEyeCfg, version 1.1

    installType
    imageType
    expandAbility
    rotationAngle

`installType` is presumably the mounting orientation, ceiling against wall against ground,
which is the setting that decides how a fisheye image should be dewarped at all.

### fishEyeSubChnCtrl

    screenNumber
    command      one of: left, right, up, down, reset

This settles what sub-channel control means. It is not a stored configuration, it is
navigation: a screen number and a direction. The camera produces dewarped views and this pans
and tilts them, with a reset to return to centre. That is a virtual PTZ over a fixed fisheye,
which is exactly the capability a client would otherwise fake by cropping regions out of the
raw image and transcoding them.

### BinoStitch, version 1.1

    distance
    xpixel
    ypixel

### bino adjust request

    heightDiff
    widthDiff

### bino adjust result

    isAdjustHeight
    isAdjustWidth
    heightDiff
    heightDiffLast
    widthDiff
    widthDiffLast

Both a current and a previous value for each axis, and a flag per axis saying whether it was
adjusted. So the camera reports what an adjustment changed rather than only where it ended
up, which is what makes an automated alignment loop possible rather than just a nudge.

### bino linewidth

    linewidth
    heightDiff

## Phases B and C, recovered the same way

### HeartBeat

`netclient` carries the request body as a literal, so there is nothing to infer:

    <?xml version="1.0" encoding="UTF-8" ?><body><HeartBeat version="1.1"></HeartBeat></body>

An empty element. The reply is not empty, and `nets_param_heart_beat_x2s` parses:

    size
    sec
    usec
    overlapCount
    delay

So this is not a bare ping. The camera returns timing and an overlap count, which makes it a
round trip measurement rather than a liveness poke. The firmware also logs
`heartbeat timeout, session:%d chn:%d devname:%s disconnected`, which is the camera side of
the session timeout this project keeps running into from the client side.

There is also `HEART_BEAT_V20` and a `support_mod_heartbeat` capability, so behaviour likely
differs by firmware and should be probed rather than assumed.

### GopCfg

    channel
    gopTime
    streamType

Three fields, and `gopTime` rather than a frame count, so the interval is expressed in time.
Worth knowing before implementing: a client joins at a keyframe, so GOP length is join
latency, and this is the setting that turns that from inherited to chosen.

## What is still missing for all four phases

**The numeric message ids.** ~~Not recoverable from the firmware by the techniques used
here.~~ They were, and the paragraph that said otherwise is left below in strikethrough
because the mistake is worth keeping.

The search looked for the parameter strings and asked what referenced them. Nothing did,
in any followable pattern, and that was read as "the ids are not in here". The dispatch
table is keyed the other way round: a record per message, id first, with the name as an
inline 32 byte field rather than a pointer to the string pool. Searching for the strings
could never find it. Anchoring on a known record and walking the stride does, and gives
all of them at once.

~~None of the ids for `HeartBeat`, `GopCfg`, `fishEyeCfg`, `fishEyeSubChnCtrl` or
`BinoStitch` appear in the Wireshark dissector, which names 121 of them, and they are not
recoverable from the firmware by the techniques used here. One capture settles all five at
once.~~

A capture is still the only source for the XML *schemas*, which no dispatch table
contains. But the schema for any message with a matching read comes from the camera
itself: read the document, change a field, send it back.

## Suggested order

1. Query the ability list on a fisheye camera and a panoramic camera. Confirm `fishEye` and
   `binoCfg` are advertised, and see what else is. Cheap, needs no capture, and it validates
   the approach before effort goes into it.
2. Confirm the field names above against one real exchange each. They came from the code that
   writes the XML, so they should match, but the encoding around them, the message ids, the
   ordering and which fields are optional are all still unknown. A single capture of
   `GET_FISH_EYE_CFG` settles most of it.
3. Implement `fishEyeSubChnCtrl` first among the write paths. It is a small payload, a screen
   number and a direction, and it is the one with a visible result: a view that moves. That
   makes it far easier to tell working from nearly working than a stitching parameter whose
   effect is subtle.
4. Stitching after that. More interesting, less useful day to day: a pano that is already
   aligned does not need realigning, whereas dewarped sub-channels would remove real work
   from the pipeline continuously.

## Tooling note

`tools/armstrings.py` resolves the string literals an ARM function references in a position
independent shared object. That is what recovered the field names above.

The naive approach does not work and it fails quietly, which is worth knowing before someone
repeats it. Position independent code keeps an offset in the literal pool rather than an
address, and adds PC at runtime. Reading the literal directly gives a number that often lands
inside some other section and decodes as a plausible looking string, so the first attempt
here produced confident nonsense out of `.dynstr` rather than an obvious failure.

`libnetpublic.so` is ARM 32 bit and unstripped, with every symbol mentioned in this document
present and its address known, so any other parameter can be read the same way.

The rule still stands regardless: this says what to look for. Confirm on the wire, implement
from the capture.

## Both features turned out to be reachable over plain HTTP, 2026-09-08

Before implementing any of this over Baichuan, check the camera's own HTTP API. Both
features are already exposed there, on cameras that have port 80 open, and no message ids
are needed.

Login returns a token, and the token goes in the query string. Passing the password in the
query string does not work, which reads as "password wrong" and looks like a credential
problem rather than a protocol one.

    POST /cgi-bin/api.cgi?cmd=Login
    [{"cmd":"Login","action":0,"param":{"User":{"userName":"admin","password":"..."}}}]

### GetStitch and SetStitch, measured on a dual lens camera

    value:   distance 10.0, stitchXMove 3, stitchYMove 4
    initial: distance  8.0, stitchXMove 0, stitchYMove 0
    range:   distance 2.0 to 20.0, stitchXMove and stitchYMove -100 to 100

The response carries the current value, the factory default and the valid range together,
which is everything needed to adjust it safely without guessing at bounds.

### GetFishEye and SetFishEye, measured on a fisheye camera

    value:   imageType 0, installType 1, rotationAngle 0
    range:   imageType 0 to 3, installType 0 to 2, rotationAngle -45 to 45

The field names match what the firmware serialiser builds exactly, which is a useful
cross check: `installType`, `imageType` and `rotationAngle` were read out of
`net_fish_eye_cfg_s2x` before any of this was queried.

The command is `GetFishEye`, not `GetFishEyeCfg`. The latter returns `notsupport`.

### Ask the camera rather than guessing

`GetAbility` reports per channel capabilities including `supportBinoStitch` and
`supportFishEyeCfg`, each with a permit and a version. On the fisheye here,
`supportFishEyeCfg` is permit 6 version 1 while `supportBinoStitch` is permit 0, which is
correct: it is a single lens camera. Unsupported commands return `ability error` rather
than failing obscurely.

### What this means for the Baichuan work

Implementing these over Baichuan is now optional rather than necessary, and the case for
doing it is narrower: a camera whose HTTP API is disabled or unreachable, or wanting one
transport for everything.

The ids are no longer the obstacle: `fishEyeCfg` is 443/444, `fishEyeSubChnCtrl` is 541,
`BinoStitch` is 417/418. The obstacle turned out to be that this firmware serves these
over CGI regardless. The floodlight is the proven case: message 288 takes the right
element name, answers 200, and does nothing observable. Expect the same here and test the
effect rather than the reply.

`expandAbility`, which appears in the firmware's fisheye serialiser, is not in the HTTP
response. So the HTTP surface is not necessarily the whole parameter, and Baichuan may
still reach settings the CGI API does not expose. That is worth checking before concluding
the two are equivalent.
