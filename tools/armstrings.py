"""Resolve string literals referenced by an ARM function in a PIC shared object.

Position independent code does not put an absolute string address in the
literal pool. It stores an offset, loads it, and adds PC:

    ldr r3, [pc, #N]     r3 = offset constant from the literal pool
    add r3, pc, r3       r3 = (address of this add + 8) + offset

So resolving a field name means pairing each load with the add that consumes
the same register, which is why a naive read of the literal pool lands in
whatever section the raw offset happens to point at.
"""
import struct
import sys

from capstone import Cs, CS_ARCH_ARM, CS_MODE_ARM


def read_sections(path):
    data = open(path, "rb").read()
    e_shoff = struct.unpack_from("<I", data, 0x20)[0]
    e_shentsize = struct.unpack_from("<H", data, 0x2E)[0]
    e_shnum = struct.unpack_from("<H", data, 0x30)[0]
    e_shstrndx = struct.unpack_from("<H", data, 0x32)[0]

    def sh(i):
        off = e_shoff + i * e_shentsize
        name, _t, _f, addr, offset, size = struct.unpack_from("<IIIIII", data, off)
        return name, addr, offset, size

    _, _, stroff, _ = sh(e_shstrndx)
    sections = {}
    for i in range(e_shnum):
        nameoff, addr, offset, size = sh(i)
        end = data.index(b"\0", stroff + nameoff)
        sections[data[stroff + nameoff:end].decode()] = (addr, offset, size)
    return data, sections


def at(data, sections, addr, length):
    for _name, (saddr, soff, ssize) in sections.items():
        if saddr and saddr <= addr < saddr + ssize:
            start = soff + (addr - saddr)
            return data[start:start + length]
    return None


def cstring(data, sections, addr):
    raw = at(data, sections, addr, 200)
    if not raw:
        return None
    end = raw.find(b"\0")
    if end <= 0 or end > 120:
        return None
    try:
        s = raw[:end].decode("ascii")
    except UnicodeDecodeError:
        return None
    if all(32 <= ord(c) < 127 for c in s):
        return s
    return None


def main(path, func_addr, func_size):
    data, sections = read_sections(path)
    code = at(data, sections, func_addr, func_size)
    if code is None:
        print("address not in any section")
        return

    md = Cs(CS_ARCH_ARM, CS_MODE_ARM)
    insns = list(md.disasm(code, func_addr))

    # register -> offset constant loaded from the literal pool
    pending = {}
    found = []
    for insn in insns:
        m, op = insn.mnemonic, insn.op_str
        if m == "ldr" and "[pc" in op:
            reg = op.split(",")[0].strip()
            try:
                imm = int(op.split("#")[-1].rstrip("]"), 0)
            except ValueError:
                continue
            lit = (insn.address + 8 + imm) & ~3
            word = at(data, sections, lit, 4)
            if word and len(word) == 4:
                pending[reg] = struct.unpack("<I", word)[0]
        elif m == "add" and "pc" in op:
            parts = [p.strip() for p in op.split(",")]
            if len(parts) >= 3 and parts[1] == "pc":
                dst, src = parts[0], parts[2]
                if src in pending:
                    target = (insn.address + 8 + pending[src]) & 0xFFFFFFFF
                    s = cstring(data, sections, target)
                    if s and len(s) >= 2:
                        found.append((hex(insn.address), s))
                    pending.pop(src, None)

    if not found:
        print("no PIC string references resolved")
        return
    print(f"{len(found)} resolved:")
    seen = set()
    for addr, s in found:
        if s not in seen:
            seen.add(s)
            print(f"  {addr}  {s!r}")


if __name__ == "__main__":
    main(sys.argv[1], int(sys.argv[2], 0), int(sys.argv[3], 0))
