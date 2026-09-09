# RTSP output

Design, 2026-09-09. Not implemented yet.

RTSP is added for compatibility with software that cannot consume anything else:
Blue Iris, Synology Surveillance Station, older NVRs, anything that takes an
RTSP URL and nothing more. It is not an upgrade to the HTTP MPEG-TS path and
does not replace it. Both run, permanently, and neither knows the other exists.

## Why not replace HTTP

HTTP is the more forgiving transport and it is worth writing down why, because
the temptation to consolidate on one will come back.

HTTP is stateless: one long-lived GET, and a client that loses it reconnects
with a single request. MPEG-TS is self-resynchronising by design, so a client
can recover mid-stream from sync bytes and repeating PAT/PMT. RTSP carries
session state that a network blip can leave half open until a timeout expires,
and reconnecting costs a full OPTIONS/DESCRIBE/SETUP/PLAY handshake.

RTSP is better at one thing: telling a client that a stream has died. RTCP and
keepalives surface a stall quickly, where a stalled TS stream looks alive until
the consumer's own timeout fires. That is the same "connected but silent" trap
already fixed on the camera side, facing the other way.

Running both keeps both properties. There is no primary and no failover.

## Shape

Additive. The camera path is not refactored.

`stream.Run` already holds a `baichuan.Frame` before muxing, carrying `Kind`,
`Codec`, `Micros` and `Data`. RTSP taps it there rather than from the hub,
because the hub carries TS bytes and demuxing them back into frames would
reintroduce exactly the parsing guesswork this project exists to avoid.

    // nil unless RTSP is configured for this stream
    type FrameSink interface{ Frame(baichuan.Frame) }

One `if sink != nil` next to the existing `h.PublishKey(pkt, key)`. With RTSP
unconfigured the daemon runs the same lines in the same order as today.

The sink must never block or slow the camera goroutine. It follows the hub's
discipline: a slow RTSP client is disconnected, never buffered. That rule is
what keeps a consumer from wedging a camera, and it is not negotiable here.

RTP timestamps come from `Micros`, the camera's own clock, which is the same
source the TS muxer already uses. Nothing invents timing.

## Package

`internal/rtsp`, wrapping `gortsplib`. One server stream per camera stream,
formats declared from the camera's own `Codec` (H265 or H264) plus MPEG4Audio
where AAC is present. Paths mirror the HTTP endpoints:

    rtsp://host:8554/<cam>          main
    rtsp://host:8554/<cam>_sub      sub
    rtsp://host:8554/<cam>_extern   extern

**TCP interleaved is the default transport.** UDP is supported for clients that
insist on it and is not offered first: it loses packets silently, and a
corrupted frame with no retransmit is worse than a slightly larger header.

Packetisation is gated on reader count. A configured stream with nobody
connected costs a branch. This is deliberately different from the TS path,
which muxes continuously so a new HTTP client can start instantly; RTSP does
not need that, because the camera connection is already held either way.

## The dependency

`gortsplib` v5, MIT. Verified rather than assumed:

- v4 on the module proxy is an empty stub containing no code. Unusable.
- v5 requires Go 1.26. The toolchain downloads it automatically, so it builds
  here on 1.23.6, but the module's `go` directive and the Dockerfile builder
  image both move to 1.26, and that becomes the minimum for building from
  source.
- Builds with `CGO_ENABLED=0`, so the static binary is unaffected.
- 11 modules link into the binary: gortsplib, mediacommon, pion
  (rtp, rtcp, sdp, srtp, logging, randutil, transport), gorilla/websocket,
  google/uuid. Every one is MIT or BSD-3. No copyleft, so the commercial
  licence is unaffected. Each licence file was read, not inferred.

This takes the dependency list from one to eleven. Hand-rolling RFC 6184,
7798 and 3640 plus the client-quirk long tail is where a from-scratch server is
weakest, and compatibility is the entire reason for building this, so the trade
is accepted deliberately.

## Configuration

    [rtsp]
    listen = ":8554"        # absent means RTSP is off entirely

    [[camera]]
    rtsp = ["main", "sub"]  # absent means this camera is not served over RTSP

Off unless asked for. Unknown keys stay a startup error.

## Testing

Unit: frames reach the sink; a nil sink is a no-op; a slow reader is dropped
rather than buffered; RTP timestamps derive from `Micros` and not from arrival
time.

Integration: the fake camera driving a real `gortsplib` client through
SETUP and PLAY, and a teardown that leaves no session behind.

Live, and this is the gate that matters: ffprobe and VLC against a real camera,
then the pano specifically. The pano is the camera whose own RTSP server
produced 147 bad segments out of 178, so HEVC over RTSP is where this is most
likely to fail. A decode error count is a measurement, not a judgement, and it
decides whether this ships.

## Rollout

1. Build additively. HTTP untouched throughout.
2. Point one camera's recorder config at RTSP. Soak overnight beside the others.
3. Compare decode errors, especially on the pano.
4. Moving the rest is a config change, or never. Nothing forces it.

Finding a problem on one camera is the point of doing it in that order.

## Not in scope

A live view in a browser. No browser plays RTSP, and HEVC over WebRTC does not
work either, so RTSP does not advance that goal and should not be justified by
it. A configuration and status interface is a separate design.
