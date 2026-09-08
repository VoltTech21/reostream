# Baichuan, as observed on the wire

What we learned implementing reostream, verified against captures from real cameras.
Where this disagrees with `neolink/dissector/protocol.md`, this file is what the cameras
actually did (firmware v3.5.1, 2026-09).

## Handshake

| # | Dir | ID | Class | Body |
|---|-----|----|-------|------|
| 1 | C→S | 1 | `0x6514` | **empty**: an encryption-negotiation probe, bytes 16/17 = `0x12`/`0xdc` |
| 2 | S→C | 1 | `0x6614` | BC-encrypted XML: `<Encryption><type>md5</type><nonce>…</nonce>` |
| 3 | C→S | 1 | `0x6414` | BC-encrypted XML: `<LoginUser>` |
| 4 | S→C | 1 | `0x0000` | BC-encrypted XML: `<DeviceInfo>` |
| 5+ | both | any | n/a | **AES** from here on |

The docs describe a legacy login carrying MD5 credentials as the first message. That
message exists but is not on the path to a working session; the probe is. The docs also
describe negotiation values `0x01`–`0x03`; this firmware uses `0x12`.

Credentials are `MD5(value + nonce)`, uppercase hex, **truncated to 31 characters**, with
a `<userVer>1</userVer>` alongside.

## Encryption

- **BC cipher** (handshake only): XOR with key `1f2d3c4b5a6978ff`, rotated by the header's
  encryption offset, every byte also XORed with the low byte of that offset.
- **AES** (everything after login): AES-128-CFB, IV `0123456789abcdef`, key = the first 16
  characters of the uppercase hex `MD5(nonce + "-" + password)`.

The encryption-offset field is structured: `[channel, stream, counter, handle]`
little-endian, where the counter increments once per outbound request and the stream id
is 0 main, 1 sub, 4 extern.

## Media framing

This is the part no document describes, and getting it wrong looks like it almost works.

**A media packet begins at the start of a message that carries an extension header, and
continues through following messages that carry none.** Bytes between the packet's
declared end and the end of its final message are filler.

Do not byte-scan for packet magics to resynchronise. Filler can contain a byte sequence
that looks like a magic, and large frames span many messages. Framing on the message
model gives zero resync on both codecs; scanning gave 1,137 stray bytes on a 900 KB H.264
capture and 22,667 on 8 MB of HEVC.

**Payloads are partly encrypted.** A media message's extension XML carries
`<encryptLen>N</encryptLen>`: the first N bytes of that message's payload are AES
encrypted, the rest is plaintext. Decrypting them turns random-looking bytes into the
`00dcH264` / `05wb` magics.

**HEVC frames carry a proprietary prefix.** Before the first NAL start code there are
80, 112 or 152 bytes (varies per frame) of camera metadata. H.264 frames have none.
Passing the prefix to a decoder produces sporadic `cu_qp_delta out of range` errors:
trimming to the first start code took a 20-second HEVC capture from 13 decode errors to 1.

Packet header layouts are as `mediapacket.md` describes: I frames 32 bytes, P frames 24,
audio 8, info variable. The camera's `microseconds` field is real and monotonic: emit it.

## Streams

`mainStream`, `subStream` and `externStream`. The third is the "balanced" stream, absent
from the Reolink UI, and it works: 896x512 H.264 on a camera whose main stream is
3840x2160 HEVC. The firmware also carries `EXTERNSTREAM_720P_SET`, so its resolution is
settable.

**One connection per stream, not per camera.** Main, sub and extern can be streamed
simultaneously.

## Sessions

Closing must send the stream-stop message; that is what releases the camera's session.
A client that vanishes without it leaves that stream refusing connections for minutes.
Verified: three back-to-back connect/stream/close cycles with no delay all succeeded.

The camera times out a session it stops hearing from (its firmware logs
`session:%u login timeout`), and the official NVR heartbeats continuously. A ping every
10s is sufficient today; a proper `HeartBeat` is the next protocol job.
