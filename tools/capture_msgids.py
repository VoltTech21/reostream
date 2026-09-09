"""Read Baichuan message ids and their XML off a packet capture.

Everything about the remaining protocol phases is known except the numeric
message ids: the parameter names, every field, and in one case the literal
request body. The ids are not in a table in either the NVR or the camera
firmware, they live in dispatch code, so a capture is the cheap way to get
them.

Usage:

    sudo tcpdump -i any -s 0 -w /tmp/bc.pcap 'host <camera> and port 9000'
    # drive the phone or desktop client, touch the setting you care about
    python3 tools/capture_msgids.py /tmp/bc.pcap

It prints each message id seen with the XML root element that travelled with
it, which is the mapping that is missing. Handshake messages are encrypted
with the BC XOR cipher and the rest with AES, so plaintext XML only appears
for messages sent before the switch, or when a key is supplied. Even without
decryption the id sequence plus payload sizes is often enough to identify
which id carried which request, because you know what you clicked and when.

Only the header is parsed here. That is deliberate: header layout is stable
and documented in docs/protocol.md, whereas payload handling belongs in the
client where it is already tested.
"""
import re
import struct
import sys
from collections import Counter, defaultdict

MAGIC_CLIENT = bytes([0xF0, 0xDE, 0xBC, 0x0A])
MAGIC_SERVER = bytes([0xF0, 0xDE, 0xBC, 0x0A])


def pcap_payloads(path):
    """Yield TCP payload bytes from a pcap or pcapng, in order.

    Handles the LINUX_SLL2 linktype tcpdump produces for `-i any`, which has a
    20 byte header rather than an Ethernet one, because that is what a capture
    taken on this network will be.
    """
    data = open(path, "rb").read()
    if data[:4] == b"\xa1\xb2\xc3\xd4" or data[:4] == b"\xd4\xc3\xb2\xa1":
        big = data[:4] == b"\xa1\xb2\xc3\xd4"
        end = ">" if big else "<"
        linktype = struct.unpack_from(end + "I", data, 20)[0]
        off = 24
        while off + 16 <= len(data):
            _ts, _us, caplen, _origlen = struct.unpack_from(end + "IIII", data, off)
            off += 16
            frame = data[off:off + caplen]
            off += caplen
            payload = strip_headers(frame, linktype)
            if payload:
                yield payload
    else:
        raise SystemExit("only classic pcap is handled; capture with -w and no --pcapng")


def strip_headers(frame, linktype):
    if linktype == 276:      # LINUX_SLL2
        frame = frame[20:]
    elif linktype == 113:    # LINUX_SLL
        frame = frame[16:]
    elif linktype == 1:      # Ethernet
        frame = frame[14:]
    else:
        return None
    if len(frame) < 20 or (frame[0] >> 4) != 4:
        return None
    ihl = (frame[0] & 0x0F) * 4
    if frame[9] != 6:        # not TCP
        return None
    tcp = frame[ihl:]
    if len(tcp) < 20:
        return None
    doff = (tcp[12] >> 4) * 4
    return tcp[doff:]


def main(path):
    seen = Counter()
    roots = defaultdict(Counter)
    order = []

    for payload in pcap_payloads(path):
        i = payload.find(MAGIC_CLIENT)
        while i != -1:
            head = payload[i:i + 24]
            if len(head) >= 20:
                msg_id = struct.unpack_from("<I", head, 4)[0]
                msg_len = struct.unpack_from("<I", head, 8)[0]
                if 0 < msg_id < 1000 and msg_len < 10_000_000:
                    seen[msg_id] += 1
                    if not order or order[-1] != msg_id:
                        order.append(msg_id)
                    body = payload[i + 24:i + 24 + min(msg_len, 4096)]
                    m = re.search(rb"<body>\s*<([A-Za-z][A-Za-z0-9_]*)", body)
                    if m:
                        roots[msg_id][m.group(1).decode()] += 1
            i = payload.find(MAGIC_CLIENT, i + 1)

    if not seen:
        print("no Baichuan messages found. Check the capture filter and that")
        print("traffic actually flowed while it was running.")
        return

    print(f"{len(seen)} distinct message ids seen\n")
    print(f"{'id':>5}  {'count':>6}  xml root elements (blank means encrypted)")
    for mid, n in sorted(seen.items()):
        named = ", ".join(f"{r} x{c}" for r, c in roots[mid].most_common(3))
        print(f"{mid:>5}  {n:>6}  {named}")

    print("\nfirst 30 ids in the order they appeared:")
    print("  " + " ".join(str(m) for m in order[:30]))
    print("\nIf you clicked one setting during the capture, the id you want is")
    print("almost certainly one that appears once, late, and is not in the")
    print("handshake or keepalive traffic.")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    main(sys.argv[1])
