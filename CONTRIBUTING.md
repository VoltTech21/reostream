# Contributing

## Bug reports and ideas

Always welcome, no paperwork. Describing a problem, explaining what you think is causing
it, or asking for a feature costs you nothing and commits you to nothing.

Useful things to include for a streaming bug: the camera model and firmware version, which
stream (main, sub or extern), what `GET /api/status` says for that camera at the time, and
the ffmpeg or ffprobe output if a client is complaining.

## Code

Pull requests need a signed contributor licence agreement. See `CLA.md`. A bot will ask
you to sign the first time you open one, and it never comes up again.

The reason is that this project is AGPL with commercial licences sold separately, which
only works while one person holds the copyright to all of it. Without the agreement, your
patch could never be included in a commercially licensed build, and the practical result
would be that it could not be merged at all.

## Working on the protocol

The Baichuan implementation is written from packet captures. That is deliberate and it has
to stay that way:

- Implement from captures, from `docs/protocol.md`, and from Neolink's Wireshark
  dissector.
- Do not copy Neolink source, and do not vendor its crates.
- Camera firmware is useful for learning what a message is called. Do not transcribe
  decompiler output into this repository.

Anything that would muddy the provenance of the code cannot be merged, however good it is.

## Tests

Protocol changes need a fixture. `tools/extract_fixture.py` pulls a capture into the shape
`internal/baichuan/testdata` expects, so tests run without a camera present.

```bash
go test ./...
```
