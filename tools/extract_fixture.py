#!/usr/bin/env python3
"""Split a Baichuan pcap into client->server and server->client TCP byte streams."""
import struct, sys, collections


def pcap_packets(path):
    data = open(path, "rb").read()
    magic = data[:4]
    if magic == b"\xd4\xc3\xb2\xa1":
        endian = "<"
    elif magic == b"\xa1\xb2\xc3\xd4":
        endian = ">"
    elif magic == b"\x4d\x3c\xb2\xa1":
        endian = "<"
    else:
        sys.exit("not a classic pcap; capture with tcpdump -w")
    linktype = struct.unpack_from(endian + "I", data, 20)[0]
    off = 24
    while off + 16 <= len(data):
        _, _, caplen, _ = struct.unpack_from(endian + "IIII", data, off)
        yield linktype, data[off + 16: off + 16 + caplen]
        off += 16 + caplen


def parse(linktype, pkt):
    # Link-layer header size: LINUX_SLL2 (tcpdump -i any on modern libpcap)
    # is 20 bytes, LINUX_SLL is 16, Ethernet is 14.
    off = {276: 20, 113: 16}.get(linktype, 14)
    if len(pkt) < off + 20:
        return None
    ver_ihl = pkt[off]
    if ver_ihl >> 4 != 4:
        return None
    ihl = (ver_ihl & 0xF) * 4
    if pkt[off + 9] != 6:
        return None
    src = ".".join(str(b) for b in pkt[off + 12: off + 16])
    t = off + ihl
    seq = struct.unpack_from(">I", pkt, t + 4)[0]
    doff = (pkt[t + 12] >> 4) * 4
    return src, seq, pkt[t + doff:]


def main(path, cam_ip, out_prefix):
    streams = collections.defaultdict(dict)
    for linktype, pkt in pcap_packets(path):
        r = parse(linktype, pkt)
        if not r:
            continue
        src, seq, payload = r
        if not payload:
            continue
        key = "s2c" if src == cam_ip else "c2s"
        streams[key][seq] = payload  # dedupe retransmits by sequence number
    for key, segs in streams.items():
        buf = b"".join(segs[s] for s in sorted(segs))
        out = f"{out_prefix}_{key}.bin"
        open(out, "wb").write(buf)
        print(f"{out}: {len(buf)} bytes")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], sys.argv[3])
