# Fixtures

Captured 2026-09-04 from a camera at 192.0.2.18, substream, using the
`neolink-cat` reference client under tcpdump. The camera's Baichuan substream was
free at the time; production was not interrupted.

- `login_c2s.bin` (2,622 bytes) / `login_s2c.bin` (65,536 bytes): the full login
  handshake, both directions.
- `stream_c2s.bin` (2,622 bytes): the whole client side of the streaming session,
  identical to the login request since the client sends nothing further once
  streaming starts.
- `stream_s2c.bin` (905,701 bytes): the server side of that session, H.264
  substream media following the handshake.
- `h265_s2c.bin` (3,000,000 bytes): a separate capture of the main stream, HEVC
  3840x2160, from a different camera. This is the fixture `h265_test.go` uses to
  exercise frames large enough to span many messages, and the trailing filler
  the camera leaves between a packet's declared end and the end of its final
  message; the other fixtures never trigger either case.

The login nonce in these fixtures is `6a9b50e3-07t2qQdtEzXpGmKi9iW7`. The camera has
an empty password, so the only credential material present is the MD5 of `admin` and
the MD5 of the reference client's default password, both public constants. Nothing
secret is committed here.
