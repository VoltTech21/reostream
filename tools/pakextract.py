"""Split a Reolink .pak firmware image into its sections.

The format is a short header followed by a table of fixed size entries, each
naming one section and giving its offset and length in the file. Nothing is
compressed at this level; the sections themselves are usually filesystem
images, so the useful ones need a second pass with unsquashfs or similar.

    python3 tools/pakextract.py FIRMWARE.pak OUTDIR

Written for static analysis of models we do not own. It does not decode or
repack anything, and nothing here is capable of producing a flashable image.
"""
import os
import struct
import sys

MAGIC = bytes([0x13, 0x59, 0x72, 0x32])
ENTRY = 64
TABLE_START = 12


def entries(blob):
    """Yield (name, version, offset, size) for each section."""
    first_data = None
    i = 0
    while True:
        base = TABLE_START + i * ENTRY
        if base + ENTRY > len(blob):
            return
        # The table runs until the first section's data begins.
        if first_data is not None and base >= first_data:
            return

        raw = blob[base:base + ENTRY]
        name = raw[0:32].split(b"\0")[0].decode("ascii", "replace")
        version = raw[32:52].split(b"\0")[0].decode("ascii", "replace")
        offset, size = struct.unpack_from("<II", raw, 56)

        # The table carries blank entries between real ones. Skipping them
        # rather than stopping is the difference between reading two sections
        # and reading all of them.
        if not name or offset == 0 or offset + size > len(blob):
            i += 1
            continue
        if first_data is None:
            first_data = offset
        yield name, version, offset, size
        i += 1


def main(path, outdir):
    blob = open(path, "rb").read()
    if blob[:4] != MAGIC:
        sys.exit(f"{path}: not a Reolink pak (magic {blob[:4].hex()})")

    os.makedirs(outdir, exist_ok=True)
    print(f"{os.path.basename(path)}: {len(blob)} bytes")
    for name, version, offset, size in entries(blob):
        safe = name.replace("/", "_")
        out = os.path.join(outdir, safe + ".bin")
        with open(out, "wb") as fh:
            fh.write(blob[offset:offset + size])
        head = blob[offset:offset + 4]
        print(f"  {name:16s} {version:10s} {size:10d} bytes  magic {head.hex()}  -> {safe}.bin")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
