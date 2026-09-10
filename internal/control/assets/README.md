# Vendored assets

This directory holds the only third party file in this repository.

## mpegts.js

- Version: 1.8.2
- Source: `dist/mpegts.js` from the npm package `mpegts.js@1.8.2`
- Fetched from: https://cdn.jsdelivr.net/npm/mpegts.js@1.8.2/dist/mpegts.js
- License: Apache License 2.0. The full text as fetched from the same
  pinned source (`https://cdn.jsdelivr.net/npm/mpegts.js@1.8.2/LICENSE`,
  verified byte-identical to `https://raw.githubusercontent.com/xqq/mpegts.js/master/LICENSE`)
  is committed verbatim at `internal/control/assets/LICENSE-mpegts.txt`,
  SHA-256 `58d1e17ffe5109a7ae296caafcadfdbe6a7d176f0bc4ab01e12a689b0499d8bd`.
  The upstream LICENSE file is the unmodified Apache-2.0 boilerplate and
  does not fill in the "[name of copyright owner]" line; the copyright
  holder per the package's own `package.json` `author` field is zheng qian
  <xqq@xqq.im> (project author/maintainer of xqq/mpegts.js).
- SHA-256 of `mpegts.js` as committed:
  `bda31748736a69cb610c2edf4623e633f1f4f47b5bda83668c8d287e51b0c3a8`
- NOTICE file: none exists upstream. The npm package file listing for
  `mpegts.js@1.8.2` has no `NOTICE` entry, and both
  `https://raw.githubusercontent.com/xqq/mpegts.js/master/NOTICE` and
  `.../NOTICE.txt` return 404. There is nothing to include under
  Apache-2.0 section 4(d) beyond the LICENSE file itself.

The upstream build is already minified (a webpack production build with a
separate `mpegts.js.LICENSE.txt` for its own bundled third party notices,
mostly es6-promise, MIT). jsdelivr's `dist/mpegts.min.js` is not a second,
more-minified build; it is jsdelivr's own passthrough wrapper around the
already-minified `dist/mpegts.js`, so `dist/mpegts.js` is the file vendored
here.

That `mpegts.js.LICENSE.txt` is itself vendored, as
`internal/control/assets/mpegts.js.LICENSE.txt`:

- Fetched from: `https://cdn.jsdelivr.net/npm/mpegts.js@1.8.2/dist/mpegts.js.LICENSE.txt`
- SHA-256 as committed:
  `dc4d5273f129801a2575dcb2649d5ad2ca06be1334c9be2a8f7929118e0e8668`
- Covers: es6-promise (MIT), Copyright (c) 2014 Yehuda Katz, Tom Dale,
  Stefan Penner and contributors. It is webpack's own generated notice
  comment, four lines long, and just names the bundled dependency and
  points at its upstream LICENSE
  (`https://raw.githubusercontent.com/stefanpenner/es6-promise/master/LICENSE`);
  it is not the full MIT text itself, and nothing longer exists upstream to
  vendor in its place -- this is the actual third party notice this bundle
  ships.

To verify it has not drifted:

```
sha256sum internal/control/assets/mpegts.js.LICENSE.txt
```

To verify the file has not drifted:

```
sha256sum internal/control/assets/mpegts.js
```

To pick up a newer version, fetch a specific pinned version from jsdelivr or
cdnjs, never a "latest" URL, and update this file's version, source URL and
checksum together with the new file -- and re-fetch mpegts.js.LICENSE.txt
alongside it, since a version bump can change what third party code is
bundled.
