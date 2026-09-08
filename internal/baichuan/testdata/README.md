# Fixtures

Captured 2026-09-04 from camera 192.0.2.18 (shop_rear), substream, using the
`neolink-cat` reference client under tcpdump. The camera's Baichuan substream was
free at the time; production was not interrupted.

- `login_c2s.bin` / `login_s2c.bin`: the first 64 KB of each direction, the full handshake.
- `stream_c2s.bin` / `stream_s2c.bin`: the whole client side, and 2 MB of media.

The login nonce in these fixtures is `6a9b50e3-07t2qQdtEzXpGmKi9iW7`. The camera has
an empty password, so the only credential material present is the MD5 of `admin` and
the MD5 of the reference client's default password, both public constants. Nothing
secret is committed here.
