# Vendored assets

This directory holds the only third party file in this repository.

## mpegts.js

- Version: 1.8.2
- Source: `dist/mpegts.js` from the npm package `mpegts.js@1.8.2`
- Fetched from: https://cdn.jsdelivr.net/npm/mpegts.js@1.8.2/dist/mpegts.js
- License: Apache License 2.0 (see the package's own `LICENSE` at
  https://github.com/xqq/mpegts.js/blob/master/LICENSE). Note: this
  license is Apache-2.0, not MIT; the package's `package.json` and its
  repository's LICENSE file both say Apache-2.0.
- SHA-256 of the file as committed:
  `bda31748736a69cb610c2edf4623e633f1f4f47b5bda83668c8d287e51b0c3a8`

The upstream build is already minified (a webpack production build with a
separate `mpegts.js.LICENSE.txt` for its own bundled third party notices,
mostly es6-promise, MIT). jsdelivr's `dist/mpegts.min.js` is not a second,
more-minified build; it is jsdelivr's own passthrough wrapper around the
already-minified `dist/mpegts.js`, so `dist/mpegts.js` is the file vendored
here.

To verify the file has not drifted:

```
sha256sum internal/control/assets/mpegts.js
```

To pick up a newer version, fetch a specific pinned version from jsdelivr or
cdnjs, never a "latest" URL, and update this file's version, source URL and
checksum together with the new file.
