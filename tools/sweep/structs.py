"""Structure maps, checksum fixups and insertion generators for the slack-space sweep.

Each format supplies:
  regions(data)        -> list of (start, end, path), covering the file, used for grouping
  fixup(orig, mut, regs, pos, n) -> mutated bytes with checksums made valid again
  inserts(data)        -> yields (label, path, make) where make(marker) returns a whole mutated file
All parsers are independent of HideMe and exist only so that results can be grouped and so
that lengths and checksums stay valid.
"""
import re
import struct
import zlib
from bisect import bisect_right

be32 = lambda v: struct.pack(">I", v)
le32 = lambda v: struct.pack("<I", v)


def locate(regs, starts, pos):
    i = bisect_right(starts, pos) - 1
    if i < 0:
        return "?"
    s, e, p = regs[i]
    return p if s <= pos < e else "gap"


# ----------------------------------------------------------------------------- ISOBMFF

CONTAINERS = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"udta", b"edts", b"dinf", b"mvex", b"moof",
              b"traf", b"mfra", b"iprp", b"ipco", b"sinf", b"schi", b"ilst", b"tref", b"rinf"}
FULL_CONTAINERS = {b"stsd": 8, b"dref": 8, b"iref": 4}
VIDEO_ENTRY = {b"avc1", b"avc3", b"hvc1", b"hev1", b"av01", b"mp4v", b"mjpg", b"jpeg", b"vp08", b"vp09", b"encv"}
AUDIO_ENTRY = {b"mp4a", b"sowt", b"twos", b"lpcm", b"alac", b"fLaC", b"Opus", b"ipcm", b"enca", b"in24", b"raw "}


class Box:
    __slots__ = ("type", "start", "hdr", "end", "path", "kids", "body")

    def __init__(self, t, s, h, e, p):
        self.type, self.start, self.hdr, self.end, self.path = t, s, h, e, p
        self.kids = None
        self.body = 0


def parse_boxes(d, start, end, path, depth=0):
    out = []
    pos = start
    while pos + 8 <= end:
        sz = struct.unpack(">I", d[pos:pos + 4])[0]
        t = d[pos + 4:pos + 8]
        h = 8
        if sz == 1:
            if pos + 16 > end:
                break
            sz = struct.unpack(">Q", d[pos + 8:pos + 16])[0]
            h = 16
        elif sz == 0:
            sz = end - pos
        if sz < h or pos + sz > end:
            break
        name = t.decode("latin1")
        b = Box(t, pos, h, pos + sz, f"{path}/{name}" if path else name)
        skip = None
        if t in CONTAINERS:
            skip = 0
        elif t in FULL_CONTAINERS:
            skip = FULL_CONTAINERS[t]
        elif t == b"meta":
            skip = 0 if d[pos + h + 4:pos + h + 8] == b"hdlr" else 4
        elif t == b"iinf":
            v = d[pos + h]
            skip = 6 if v == 0 else 8
        elif t in VIDEO_ENTRY and path.endswith("stsd"):
            skip = 78
        elif t in AUDIO_ENTRY and path.endswith("stsd"):
            ver = struct.unpack(">H", d[pos + h + 8:pos + h + 10])[0]
            skip = {0: 28, 1: 44, 2: 64}.get(ver, 28)
        elif t == b"wave":
            skip = 0
        elif path.endswith("ilst"):
            skip = 0
        if skip is not None and depth < 12 and pos + h + skip <= pos + sz:
            b.body = skip
            b.kids = parse_boxes(d, pos + h + skip, pos + sz, b.path, depth + 1)
        out.append(b)
        pos += sz
    return out


def iso_tree(d):
    return parse_boxes(d, 0, len(d), "")


def iso_regions(d):
    regs = []

    def rec(boxes):
        for b in boxes:
            if b.kids is None:
                regs.append((b.start, b.end, b.path))
            else:
                regs.append((b.start, b.start + b.hdr + b.body, b.path + "#hdr"))
                rec(b.kids)
                last = b.kids[-1].end if b.kids else b.start + b.hdr + b.body
                if last < b.end:
                    regs.append((last, b.end, b.path + "#tail"))

    tree = iso_tree(d)
    rec(tree)
    if tree and tree[-1].end < len(d):
        regs.append((tree[-1].end, len(d), "#trailing"))
    return regs


def iso_all(tree):
    for b in tree:
        yield b
        if b.kids is not None:
            yield from iso_all(b.kids)


def iso_fix_offsets(d, pos, delta):
    """Keep absolute offsets valid after inserting delta bytes at pos."""
    d = bytearray(d)
    tree = parse_boxes(bytes(d), 0, len(d), "")

    def fix_one(v):
        return v + delta if v >= pos else v

    def rec(boxes, moof=None):
        for b in boxes:
            p = b.start + b.hdr
            if b.type == b"stco":
                n = struct.unpack(">I", d[p + 4:p + 8])[0]
                for i in range(n):
                    q = p + 8 + i * 4
                    if q + 4 > b.end:
                        break
                    d[q:q + 4] = be32(fix_one(struct.unpack(">I", d[q:q + 4])[0]))
            elif b.type == b"co64":
                n = struct.unpack(">I", d[p + 4:p + 8])[0]
                for i in range(n):
                    q = p + 8 + i * 8
                    if q + 8 > b.end:
                        break
                    d[q:q + 8] = struct.pack(">Q", fix_one(struct.unpack(">Q", d[q:q + 8])[0]))
            elif b.type == b"tfhd":
                fl = struct.unpack(">I", d[p:p + 4])[0] & 0xffffff
                if fl & 1:
                    q = p + 8
                    d[q:q + 8] = struct.pack(">Q", fix_one(struct.unpack(">Q", d[q:q + 8])[0]))
            elif b.type == b"trun":
                fl = struct.unpack(">I", d[p:p + 4])[0] & 0xffffff
                if fl & 1 and moof is not None and moof.start < pos <= moof.end:
                    q = p + 8
                    v = struct.unpack(">i", d[q:q + 4])[0]
                    d[q:q + 4] = struct.pack(">i", v + delta)
            elif b.type == b"tfra":
                ver = d[p]
                sizes = struct.unpack(">I", d[p + 8:p + 12])[0]
                n = struct.unpack(">I", d[p + 12:p + 16])[0]
                lt, lr, ls = (sizes >> 4) & 3, (sizes >> 2) & 3, sizes & 3
                step = (16 if ver else 8) + lt + 1 + lr + 1 + ls + 1
                for i in range(n):
                    q = p + 16 + i * step + (8 if ver else 4)
                    if ver:
                        d[q:q + 8] = struct.pack(">Q", fix_one(struct.unpack(">Q", d[q:q + 8])[0]))
                    else:
                        d[q:q + 4] = be32(fix_one(struct.unpack(">I", d[q:q + 4])[0]))
            elif b.type == b"iloc":
                try:
                    fix_iloc(d, p, b.end, fix_one)
                except Exception:
                    pass
            if b.kids is not None:
                rec(b.kids, b if b.type == b"moof" else moof)

    rec(tree)
    return bytes(d)


def fix_iloc(d, p, end, fix_one):
    ver = d[p]
    p += 4
    os_ls = d[p]
    bs_is = d[p + 1]
    off_s, len_s, base_s, idx_s = os_ls >> 4, os_ls & 15, bs_is >> 4, bs_is & 15
    p += 2
    if ver < 2:
        n = struct.unpack(">H", d[p:p + 2])[0]
        p += 2
    else:
        n = struct.unpack(">I", d[p:p + 4])[0]
        p += 4
    for _ in range(n):
        p += 2 if ver < 2 else 4
        method = 0
        if ver in (1, 2):
            method = struct.unpack(">H", d[p:p + 2])[0] & 15
            p += 2
        p += 2  # data reference index
        base = int.from_bytes(d[p:p + base_s], "big") if base_s else 0
        base_at = p
        p += base_s
        ext = struct.unpack(">H", d[p:p + 2])[0]
        p += 2
        for j in range(ext):
            if ver in (1, 2) and idx_s:
                p += idx_s
            off = int.from_bytes(d[p:p + off_s], "big") if off_s else 0
            if method == 0:
                if base_s:
                    if j == 0:
                        nb = fix_one(base + off) - off
                        d[base_at:base_at + base_s] = nb.to_bytes(base_s, "big")
                elif off_s:
                    d[p:p + off_s] = fix_one(off).to_bytes(off_s, "big")
            p += off_s + len_s


def gen_at(b, M, i):
    return KEPT[b.type](M)[i][1]


def iso_grow(d, pos, ins, ancestors):
    out = bytearray(d[:pos] + ins + d[pos:])
    for b in ancestors:
        if b.hdr == 16:
            out[b.start + 8:b.start + 16] = struct.pack(">Q", b.end - b.start + len(ins))
        else:
            cur = struct.unpack(">I", d[b.start:b.start + 4])[0]
            if cur != 0:
                out[b.start:b.start + 4] = be32(b.end - b.start + len(ins))
    return iso_fix_offsets(bytes(out), pos, len(ins))


def _b(t, *parts):
    body = b"".join(parts)
    return be32(8 + len(body)) + t + body


def _fb(t, *parts):
    return _b(t, b"\0\0\0\0", *parts)


# Boxes on the keep lists, for each parent, as a function of the marker. Each is as valid as
# a real one would be, with the marker in the part of it readers skip or do not check.
KEPT = {
    b"stbl": lambda M: [("stsh", _fb(b"stsh", be32(1), M)), ("stdp", _fb(b"stdp", M)), ("padb", _fb(b"padb", be32(0), M)),
                        ("stps", _fb(b"stps", be32(0), M)), ("cslg", _fb(b"cslg", bytes(20), M)), ("sdtp", _fb(b"sdtp", M)),
                        ("sgpd_roll", _fb(b"sgpd", b"roll", be32(1), M[:2], M[2:])), ("sgpd_new", _fb(b"sgpd", M[:4], be32(0), M[4:])),
                        ("sbgp", _fb(b"sbgp", b"roll", be32(0), M)), ("subs", _fb(b"subs", be32(0), M)), ("ctts_tail", _fb(b"ctts", be32(0), M))],
    b"trak": lambda M: [("clip", _b(b"clip", _b(b"crgn", M))), ("matt", _b(b"matt", _fb(b"kmat", M))), ("load", _b(b"load", bytes(16), M)),
                        ("tapt", _b(b"tapt", _fb(b"clef", bytes(8), M))), ("txas", _b(b"txas", M)), ("edts", _b(b"edts", _fb(b"elst", be32(0), M))),
                        ("trgr", _b(b"trgr", _b(b"msrc", M))), ("tref", _b(b"tref", _b(b"zzzz", M)))],
    b"mdia": lambda M: [("elng", _fb(b"elng", b"en\0", M))],
    b"minf": lambda M: [("gmhd", _b(b"gmhd", _b(b"gmin", bytes(16), M))), ("gmhd_text", _b(b"gmhd", _b(b"text", bytes(36), M))), ("hmhd", _fb(b"hmhd", bytes(16), M)),
                        ("nmhd", _fb(b"nmhd", M)), ("sthd", _fb(b"sthd", M))],
    b"mvex": lambda M: [("leva", _fb(b"leva", b"\0", M)), ("mehd", _fb(b"mehd", bytes(4), M))],
    b"traf": lambda M: [("sgpd", _fb(b"sgpd", b"roll", be32(1), M[:2], M[2:])), ("sbgp", _fb(b"sbgp", b"roll", be32(0), M)), ("subs", _fb(b"subs", be32(0), M))],
    b"mfra": lambda M: [("tfra", _fb(b"tfra", be32(1), be32(0), be32(0), M)), ("mfro", _fb(b"mfro", bytes(4), M))],
    b"ipco": lambda M: [("pasp", _b(b"pasp", bytes(8), M)), ("clli", _b(b"clli", bytes(4), M)), ("mdcv", _b(b"mdcv", bytes(24), M)), ("irot", _b(b"irot", b"\0", M)),
                        ("colr", _b(b"colr", b"nclx", bytes(7), M)), ("auxC", _fb(b"auxC", b"urn:x\0", M)), ("lsel", _b(b"lsel", bytes(2), M)), ("rloc", _fb(b"rloc", bytes(8), M))],
}


def iso_inserts(d):
    tree = iso_tree(d)
    for b in iso_all(tree):
        gen = KEPT.get(b.type)
        if gen is None:
            continue
        anc = [x for x in iso_all(tree) if x.start <= b.start and b.end <= x.end]
        for i, (lab, _) in enumerate(gen(b"QQQQQQQQ")):
            yield f"kept_{lab}", f"kept:{b.path}", (lambda M, b=b, anc=tuple(anc), i=i: iso_grow(d, b.end, gen_at(b, M, i), anc))

    def rec(boxes, anc, path, start_pos):
        positions = [b.start for b in boxes]
        if boxes:
            positions.append(boxes[-1].end)
        else:
            positions.append(start_pos)
        for pos in positions:
            for lab, mk in (("box_zzzz", lambda M: be32(8 + len(M)) + b"zzzz" + M),
                            ("box_free", lambda M: be32(8 + len(M)) + b"free" + M),
                            ("box_uuid", lambda M: be32(24 + len(M)) + b"uuid" + b"\x11" * 16 + M)):
                p = path or "top"
                yield lab, f"insert:{p}", (lambda M, pos=pos, mk=mk, anc=tuple(anc): iso_grow(d, pos, mk(M), anc))
        for b in boxes:
            if b.kids is not None:
                yield from rec(b.kids, anc + [b], b.path, b.start + b.hdr + b.body)
            # Grow every box with bytes at its end. A container's tail is not a child box.
            yield "tail", f"tail:{b.path}", (lambda M, b=b, anc=tuple(anc): iso_grow(d, b.end, M, anc + (b,)))

    yield from rec(tree, [], "", 0)
    yield "raw_eof", "trailing", (lambda M: d + M)


# ----------------------------------------------------------------------------- RIFF / AIFF

def riff_parse(d, start, end, path, be, depth=0, webp=False):
    out = []
    pos = start
    fmt = ">I" if be else "<I"
    while pos + 8 <= end:
        t = d[pos:pos + 4]
        sz = struct.unpack(fmt, d[pos + 4:pos + 8])[0]
        dend = pos + 8 + sz
        if dend > end:
            dend = end
        pad = sz & 1
        name = t.decode("latin1")
        nd = {"t": t, "start": pos, "data": pos + 8, "dend": dend, "end": min(dend + pad, end), "kids": None, "skip": 0}
        if t in (b"RIFF", b"LIST", b"FORM") and depth < 6:
            form = d[pos + 8:pos + 12].decode("latin1")
            nd["path"] = f"{path}/{name}:{form}"
            nd["skip"] = 4
            nd["kids"] = riff_parse(d, pos + 12, dend, nd["path"], be, depth + 1, webp)
        elif webp and t == b"ANMF":
            nd["path"] = f"{path}/{name}"
            nd["skip"] = 16
            nd["kids"] = riff_parse(d, pos + 8 + 16, dend, nd["path"], be, depth + 1, webp)
        else:
            nd["path"] = f"{path}/{name}"
        out.append(nd)
        pos = nd["end"]
    return out


def riff_regions(d, be=False, webp=False):
    regs = []
    tree = riff_parse(d, 0, len(d), "", be, webp=webp)

    def rec(nodes):
        for n in nodes:
            if n["kids"] is None:
                regs.append((n["start"], n["end"], n["path"]))
            else:
                regs.append((n["start"], n["data"] + n["skip"], n["path"] + "#hdr"))
                rec(n["kids"])
                last = n["kids"][-1]["end"] if n["kids"] else n["data"] + n["skip"]
                if last < n["end"]:
                    regs.append((last, n["end"], n["path"] + "#tail"))

    rec(tree)
    if tree and tree[-1]["end"] < len(d):
        regs.append((tree[-1]["end"], len(d), "#trailing"))
    return regs


def riff_grow(d, pos, ins, anc, be, fix_idx=False):
    fmt = ">I" if be else "<I"
    out = bytearray(d[:pos] + ins + d[pos:])
    for n in anc:
        sz = struct.unpack(fmt, d[n["start"] + 4:n["start"] + 8])[0]
        out[n["start"] + 4:n["start"] + 8] = struct.pack(fmt, sz + len(ins))
    if fix_idx:
        out = avi_fix_idx(d, bytes(out), pos, len(ins))
    return bytes(out)


def avi_fix_idx(orig, out, pos, delta):
    """idx1 offsets are relative to the movi fourcc. Keep them valid."""
    m = out.find(b"movi")
    i = out.rfind(b"idx1")
    if m < 0 or i < 0:
        return out
    # Where was movi before the insertion?
    mo = orig.find(b"movi")
    if pos <= mo:
        return out
    rel = pos - mo
    b = bytearray(out)
    sz = struct.unpack("<I", b[i + 4:i + 8])[0]
    for k in range(sz // 16):
        q = i + 8 + k * 16 + 8
        v = struct.unpack("<I", b[q:q + 4])[0]
        if v >= rel:
            b[q:q + 4] = le32(v + delta)
    return bytes(b)


def riff_inserts(d, be=False, webp=False, avi=False):
    tree = riff_parse(d, 0, len(d), "", be, webp=webp)
    fmt = ">I" if be else "<I"
    ch = lambda t, M: t + struct.pack(fmt, len(M)) + M + (b"\0" if len(M) & 1 else b"")

    def rec(nodes, anc, path, base):
        positions = [n["start"] for n in nodes] + [nodes[-1]["end"] if nodes else base]
        for pos in positions:
            kinds = [("chunk_zzzz", lambda M: ch(b"zzzz", M)), ("chunk_JUNK", lambda M: ch(b"JUNK", M))]
            if not be:
                kinds.append(("list_INFO", lambda M: ch(b"LIST", b"INFOIZZZ" + struct.pack(fmt, len(M)) + M)))
            else:
                kinds.append(("chunk_ANNO", lambda M: ch(b"ANNO", M)))
            for lab, mk in kinds:
                yield lab, f"insert:{path or 'top'}", (lambda M, pos=pos, mk=mk, anc=tuple(anc): riff_grow(d, pos, mk(M), anc, be, avi))
        for n in nodes:
            if n["kids"] is not None:
                yield from rec(n["kids"], anc + [n], n["path"], n["data"] + n["skip"])
            yield "tail", f"tail:{n['path']}", (lambda M, n=n, anc=tuple(anc): riff_grow(d, n["dend"], M, anc + (n,), be, avi))
            # A padding byte's room: bytes in the slack after an odd chunk are covered by the overwrite pass.

    # Do not add siblings of the root chunk: those are data past the end, handled below.
    root = tree[0]
    if root["kids"] is not None:
        yield from rec(root["kids"], [root], root["path"], root["data"] + root["skip"])
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "chunk_eof", "trailing", (lambda M: d + ch(b"zzzz", M))


# ----------------------------------------------------------------------------- JPEG

def jpeg_segments(d):
    segs = []
    pos = 2
    regs = [(0, 2, "SOI")]
    while pos + 2 <= len(d):
        if d[pos] != 0xFF:
            nx = d.find(b"\xff", pos)
            if nx < 0:
                break
            regs.append((pos, nx, "garbage"))
            pos = nx
            continue
        m = d[pos + 1]
        if m == 0xFF:
            regs.append((pos, pos + 1, "fill"))
            pos += 1
            continue
        if m == 0xD9:
            regs.append((pos, pos + 2, "EOI"))
            segs.append(("EOI", pos, pos + 2))
            pos += 2
            break
        if 0xD0 <= m <= 0xD7 or m == 0x01:
            regs.append((pos, pos + 2, f"M{m:02X}"))
            pos += 2
            continue
        ln = struct.unpack(">H", d[pos + 2:pos + 4])[0]
        name = {0xE0: "APP0", 0xE1: "APP1", 0xE2: "APP2", 0xED: "APP13", 0xEE: "APP14", 0xFE: "COM", 0xDB: "DQT",
                0xC4: "DHT", 0xC0: "SOF0", 0xC2: "SOF2", 0xDD: "DRI", 0xDA: "SOS"}.get(m, f"M{m:02X}")
        end = pos + 2 + ln
        if name in ("APP1", "APP2"):
            tag = d[pos + 4:pos + 10]
            name += "/" + ("Exif" if tag.startswith(b"Exif") else "XMP" if tag.startswith(b"http") else "ICC" if tag.startswith(b"ICC") else "other")
        regs.append((pos, end, name))
        segs.append((name, pos, end))
        pos = end
        if m == 0xDA:
            # entropy data until a non-RST marker
            e = pos
            while True:
                e = d.find(b"\xff", e)
                if e < 0 or e + 1 >= len(d):
                    e = len(d)
                    break
                if d[e + 1] == 0 or 0xD0 <= d[e + 1] <= 0xD7:
                    e += 2
                    continue
                break
            regs.append((pos, e, "entropy"))
            pos = e
    if pos < len(d):
        regs.append((pos, len(d), "#trailing"))
    return segs, regs


def jpeg_regions(d):
    return jpeg_segments(d)[1]


def jpeg_inserts(d):
    segs, regs = jpeg_segments(d)
    seg = lambda m, M: bytes([0xFF, m]) + struct.pack(">H", len(M) + 2) + M
    boundaries = [2] + [s[2] for s in segs if s[0] != "SOS" and s[0] != "EOI"] + [s[1] for s in segs]
    # After each scan's entropy data, before the next marker
    boundaries = sorted(set(boundaries))
    for pos in boundaries:
        name = locate(regs, [r[0] for r in regs], pos)
        p = f"before:{name}"
        for lab, mk in (("APP15", lambda M: seg(0xEF, M)), ("COM", lambda M: seg(0xFE, M)), ("APP11", lambda M: seg(0xEB, b"zz\0" + M)),
                        ("APP4", lambda M: seg(0xE4, M)), ("JPG5", lambda M: seg(0xF5, M)), ("raw", lambda M: M), ("ff_raw", lambda M: b"\xff" + M)):
            yield lab, p, (lambda M, pos=pos, mk=mk: d[:pos] + mk(M) + d[pos:])
    # Grow each marker segment
    for name, s, e in segs:
        if name == "EOI":
            continue

        def grow(M, s=s, e=e):
            ln = struct.unpack(">H", d[s + 2:s + 4])[0]
            return d[:s + 2] + struct.pack(">H", ln + len(M)) + d[s + 4:e] + M + d[e:]

        yield "tail", f"tail:{name}", grow
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "jpeg_eof", "trailing", (lambda M: d + b"\xff\xd8" + seg(0xFE, M) + b"\xff\xd9")


# ----------------------------------------------------------------------------- PNG

def png_chunks(d):
    pos = 8
    out = []
    while pos + 12 <= len(d):
        n = struct.unpack(">I", d[pos:pos + 4])[0]
        t = d[pos + 4:pos + 8]
        if pos + 12 + n > len(d):
            break
        out.append((t.decode("latin1"), pos, pos + 12 + n))
        pos += 12 + n
        if t == b"IEND":
            break
    return out, pos


def png_regions(d):
    regs = [(0, 8, "signature")]
    ch, end = png_chunks(d)
    for t, s, e in ch:
        regs.append((s, e, t))
    if end < len(d):
        regs.append((end, len(d), "#trailing"))
    return regs


def png_fix(orig, mut, regs, pos, n):
    """Recompute CRCs of chunks the window touched, unless that would overwrite the marker."""
    out = bytearray(mut)
    for s, e, p in regs:
        if p in ("signature", "#trailing") or e <= pos or s >= pos + n:
            continue
        if pos + n > e - 4:  # marker reaches into this CRC
            continue
        out[e - 4:e] = be32(zlib.crc32(bytes(out[s + 4:e - 4])) & 0xffffffff)
    return bytes(out)


def png_inserts(d):
    ch, end = png_chunks(d)
    mk = lambda t, b: be32(len(b)) + t + b + be32(zlib.crc32(t + b) & 0xffffffff)
    for i, (t, s, e) in enumerate(ch):
        if i == 0:
            continue  # nothing may precede IHDR
        for lab, gen in (("zzZz", lambda M: mk(b"zzZz", M)), ("tEXt", lambda M: mk(b"tEXt", b"Comment\0" + M)),
                         ("iTXt", lambda M: mk(b"iTXt", b"Comment\0\0\0\0\0" + M)),
                         ("zTXt", lambda M: mk(b"zTXt", b"Comment\0\0" + zlib.compress(M))),
                         ("caNv", lambda M: mk(b"caNv", M)), ("vpAg", lambda M: mk(b"vpAg", M)), ("sTER", lambda M: mk(b"sTER", M)),
                         ("dSIG", lambda M: mk(b"dSIG", M)), ("orNT", lambda M: mk(b"orNT", M))):
            yield lab, f"before:{t}", (lambda M, s=s, gen=gen: d[:s] + gen(M) + d[s:])
    for t, s, e in ch:
        def grow(M, t=t, s=s, e=e):
            body = d[s + 8:e - 4] + M
            return d[:s] + mk(t.encode("latin1"), body) + d[e:]
        yield "tail", f"tail:{t}", grow
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "chunk_eof", "trailing", (lambda M: d + mk(b"tEXt", b"Comment\0" + M))
    # A second zlib stream's worth of data in a separate IDAT after the first stream's end
    last = [c for c in ch if c[0] == "IDAT"]
    if last:
        t, s, e = last[-1]
        yield "IDAT_extra", "after:IDAT", (lambda M: d[:e] + mk(b"IDAT", M) + d[e:])


# ----------------------------------------------------------------------------- GIF

def gif_blocks(d):
    """Returns regions, boundaries between top-level blocks, and sub-block runs."""
    regs = [(0, 6, "header"), (6, 13, "LSD")]
    pos = 13
    flags = d[10]
    if flags & 0x80:
        n = 3 * (2 << (flags & 7))
        regs.append((pos, pos + n, "GCT"))
        pos += n
    bounds = [pos]
    runs = []

    def subs(p, name):
        start = p
        while p < len(d):
            n = d[p]
            if n == 0:
                regs.append((start, p + 1, name))
                runs.append((name, start, p))
                return p + 1
            p += 1 + n
        regs.append((start, min(p, len(d)), name))
        return min(p, len(d))

    while pos < len(d):
        b = d[pos]
        if b == 0x3B:
            regs.append((pos, pos + 1, "trailer"))
            pos += 1
            break
        if b == 0x21:
            lab = d[pos + 1]
            name = {0xF9: "GCE", 0xFE: "comment", 0xFF: "appext", 0x01: "PTE"}.get(lab, "ext%02X" % lab)
            regs.append((pos, pos + 2, name + "#hdr"))
            pos = subs(pos + 2, name)
        elif b == 0x2C:
            regs.append((pos, pos + 10, "image-desc"))
            fl = d[pos + 9]
            pos += 10
            if fl & 0x80:
                n = 3 * (2 << (fl & 7))
                regs.append((pos, pos + n, "LCT"))
                pos += n
            regs.append((pos, pos + 1, "LZW-min"))
            pos = subs(pos + 1, "image-data")
        else:
            break
        bounds.append(pos)
    if pos < len(d):
        regs.append((pos, len(d), "#trailing"))
    return regs, bounds, runs


def gif_regions(d):
    return gif_blocks(d)[0]


def gif_inserts(d):
    regs, bounds, runs = gif_blocks(d)
    sb = lambda M: bytes([len(M)]) + M + b"\0"
    for pos in bounds:
        p = "boundary"
        for lab, mk in (("comment", lambda M: b"\x21\xfe" + sb(M)), ("app", lambda M: b"\x21\xff\x0bZZZZZZZZ1.0" + sb(M)),
                        ("ext_unknown", lambda M: b"\x21\x77" + sb(M)), ("raw", lambda M: M),
                        ("ptext", lambda M: b"\x21\x01\x0c\0\0\0\0\x08\0\x08\0\x08\x08\0\1" + sb(M))):
            yield lab, p, (lambda M, pos=pos, mk=mk: d[:pos] + mk(M) + d[pos:])
    for name, s, e in runs:
        # e is the block terminator. Add a sub-block before it.
        yield "subblock", f"tail:{name}", (lambda M, e=e: d[:e] + bytes([len(M)]) + M + d[e:])
    yield "raw_eof", "trailing", (lambda M: d + M)


# ----------------------------------------------------------------------------- TIFF

def tiff_regions(d):
    end = "<" if d[:2] == b"II" else ">"
    u32 = lambda b: struct.unpack(end + "I", b)[0]
    u16 = lambda b: struct.unpack(end + "H", b)[0]
    regs = [(0, 8, "header")]
    TS = {1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8, 13: 4}
    NAMES = {273: "strip", 279: "stripcounts", 324: "tile", 325: "tilecounts", 256: "w", 257: "h", 258: "bps", 262: "photo",
             282: "xres", 283: "yres", 315: "Artist", 305: "Software", 271: "Make", 272: "Model", 270: "Desc", 306: "Date",
             347: "JPEGTables", 277: "spp", 259: "compr", 278: "rowsperstrip", 284: "planar", 296: "resunit", 339: "sampleformat", 317: "predictor", 530: "ycbcrsub", 532: "ref", 34665: "ExifIFD", 34853: "GPSIFD", 34675: "ICC"}
    ifd = u32(d[4:8])
    seen = 0
    while ifd and ifd + 2 <= len(d) and seen < 16:
        n = u16(d[ifd:ifd + 2])
        regs.append((ifd, ifd + 2, f"IFD{seen}#count"))
        next_at = ifd + 2 + n * 12
        strips = counts = None
        for i in range(n):
            e = ifd + 2 + i * 12
            tag, typ, cnt = u16(d[e:e + 2]), u16(d[e + 2:e + 4]), u32(d[e + 4:e + 8])
            nm = NAMES.get(tag, f"tag{tag}")
            regs.append((e, e + 8, f"IFD{seen}/{nm}#entry"))
            sz = TS.get(typ, 1) * cnt
            if sz <= 4:
                regs.append((e + 8, e + 12, f"IFD{seen}/{nm}#inline"))
            else:
                regs.append((e + 8, e + 12, f"IFD{seen}/{nm}#offset"))
                o = u32(d[e + 8:e + 12])
                regs.append((o, o + sz, f"IFD{seen}/{nm}#value"))
            if tag in (273, 324):
                strips = (typ, cnt, e)
            if tag in (279, 325):
                counts = (typ, cnt, e)
        regs.append((next_at, next_at + 4, f"IFD{seen}#next"))
        if strips and counts:
            def vals(info):
                typ, cnt, e = info
                w = TS[typ]
                base = e + 8 if w * cnt <= 4 else u32(d[e + 8:e + 12])
                return [int.from_bytes(d[base + j * w:base + (j + 1) * w], "little" if end == "<" else "big") for j in range(cnt)]
            for o, c in zip(vals(strips), vals(counts)):
                regs.append((o, o + c, f"IFD{seen}/strip-data"))
        ifd = u32(d[next_at:next_at + 4])
        seen += 1
    regs = sorted(set(regs))
    # Fill gaps
    out = []
    p = 0
    for s, e, n in regs:
        if s > p:
            out.append((p, s, "#gap"))
        if e > p:
            out.append((max(s, p), e, n))
            p = e
    if p < len(d):
        out.append((p, len(d), "#trailing"))
    return out


def tiff_inserts(d):
    end = "<" if d[:2] == b"II" else ">"
    u32 = lambda b: struct.unpack(end + "I", b)[0]
    u16 = lambda b: struct.unpack(end + "H", b)[0]
    p32 = lambda v: struct.pack(end + "I", v)
    p16 = lambda v: struct.pack(end + "H", v)
    yield "raw_eof", "trailing", (lambda M: d + M)

    ifds = []
    ifd = u32(d[4:8])
    ptr = 4
    while ifd and ifd + 2 <= len(d) and len(ifds) < 16:
        ifds.append((ptr, ifd))
        n = u16(d[ifd:ifd + 2])
        ptr = ifd + 2 + n * 12
        ifd = u32(d[ptr:ptr + 4])

    def add_tag(M, ptr, ifd, tag, typ=7):
        n = u16(d[ifd:ifd + 2])
        ents = [d[ifd + 2 + i * 12:ifd + 14 + i * 12] for i in range(n)]
        nxt = d[ifd + 2 + n * 12:ifd + 6 + n * 12]
        base = len(d)
        if base & 1:
            base += 1
        pad = b"\0" * (base - len(d))
        val_at = base
        new = p16(tag) + p16(typ) + p32(len(M)) + p32(val_at)
        ents2 = sorted(ents + [new], key=lambda e: u16(e[:2]))
        new_ifd = val_at + len(M) + (len(M) & 1)
        out = bytearray(d + pad + M + b"\0" * (len(M) & 1) + p16(len(ents2)) + b"".join(ents2) + nxt)
        out[ptr:ptr + 4] = p32(new_ifd)
        return bytes(out)

    for i, (ptr, ifd) in enumerate(ifds):
        for tag, name in ((0xC123, "private"), (315, "Artist"), (42016, "ImageUniqueID"), (700, "XMP"), (65000, "high")):
            yield f"tag_{name}", f"IFD{i}", (lambda M, ptr=ptr, ifd=ifd, tag=tag: add_tag(M, ptr, ifd, tag))
    # Bytes between the last structure and a relocated IFD: a gap no tag refers to.
    for i, (ptr, ifd) in enumerate(ifds):
        def gap(M, ptr=ptr, ifd=ifd):
            n = u16(d[ifd:ifd + 2])
            blob = d[ifd:ifd + 6 + n * 12]
            base = len(d) + len(M)
            out = bytearray(d + M + (b"\0" if base & 1 else b"") + blob)
            out[ptr:ptr + 4] = p32(base + (base & 1))
            return bytes(out)
        yield "gap", f"gap:IFD{i}", gap
    # A second chain: a next-IFD pointer on the last IFD to a page holding only a tiny image.
    yield "unref_page", "chain", _tiff_extra_page(d, end, ifds)


def _tiff_extra_page(d, end, ifds):
    u16 = lambda b: struct.unpack(end + "H", b)[0]
    p16 = lambda v: struct.pack(end + "H", v)
    p32 = lambda v: struct.pack(end + "I", v)

    def mk(M):
        ptr, ifd = ifds[-1]
        n = u16(d[ifd:ifd + 2])
        nxt_at = ifd + 2 + n * 12
        base = len(d) + (len(d) & 1)
        strip_at = base
        ifd_at = strip_at + len(M) + (len(M) & 1)
        ents = [(256, 4, 1, len(M)), (257, 4, 1, 1), (258, 3, 1, 8), (259, 3, 1, 1), (262, 3, 1, 1), (273, 4, 1, strip_at),
                (277, 3, 1, 1), (278, 4, 1, 1), (279, 4, 1, len(M))]
        blob = p16(len(ents))
        for t, ty, c, v in ents:
            blob += p16(t) + p16(ty) + p32(c) + (p16(v) + p16(0) if ty == 3 else p32(v))
        blob += p32(0)
        out = bytearray(d + b"\0" * (base - len(d)) + M + b"\0" * (len(M) & 1) + blob)
        out[nxt_at:nxt_at + 4] = p32(ifd_at)
        return bytes(out)

    return mk


# ----------------------------------------------------------------------------- FLAC

def crc8(b):
    c = 0
    for x in b:
        c ^= x
        for _ in range(8):
            c = ((c << 1) ^ 7) & 0xFF if c & 0x80 else (c << 1) & 0xFF
    return c


def crc16(b):
    c = 0
    for x in b:
        c ^= x << 8
        for _ in range(8):
            c = ((c << 1) ^ 0x8005) & 0xFFFF if c & 0x8000 else (c << 1) & 0xFFFF
    return c


def flac_meta(d):
    pos = 4
    out = []
    while pos + 4 <= len(d):
        h = d[pos]
        last = h & 0x80
        t = h & 0x7f
        n = int.from_bytes(d[pos + 1:pos + 4], "big")
        out.append((t, pos, pos + 4 + n, bool(last)))
        pos += 4 + n
        if last:
            break
    return out, pos


def flac_frames(d, start):
    """Find frames by checking CRC-8 and CRC-16."""
    frames = []
    i = start
    while i + 4 < len(d):
        i = d.find(b"\xff", i)
        if i < 0 or i + 4 >= len(d):
            break
        if d[i + 1] not in (0xF8, 0xF9):
            i += 1
            continue
        # try a header of up to 16 bytes
        ok = False
        for hl in range(5, 17):
            if i + hl > len(d):
                break
            if crc8(d[i:i + hl - 1]) == d[i + hl - 1]:
                # search end
                j = i + hl + 2
                while j <= len(d):
                    if j == len(d) or (d[j] == 0xFF and d[j + 1:j + 2] in (b"\xf8", b"\xf9")):
                        if crc16(d[i:j - 2]) == struct.unpack(">H", d[j - 2:j])[0]:
                            frames.append((i, j, hl))
                            ok = True
                            break
                    j += 1
                if ok:
                    break
        if ok:
            i = frames[-1][1]
        else:
            i += 1
    return frames


FLAC_NAMES = {0: "STREAMINFO", 1: "PADDING", 2: "APPLICATION", 3: "SEEKTABLE", 4: "VORBIS_COMMENT", 5: "CUESHEET", 6: "PICTURE"}


def flac_regions(d):
    regs = [(0, 4, "magic")]
    meta, end = flac_meta(d)
    for t, s, e, _ in meta:
        regs.append((s, e, FLAC_NAMES.get(t, f"type{t}")))
    fr = flac_frames(d, end)
    last = end
    for s, e, hl in fr:
        if s > last:
            regs.append((last, s, "#between-frames"))
        regs.append((s, s + hl, "frame-hdr"))
        regs.append((s + hl, e - 2, "frame-audio"))
        regs.append((e - 2, e, "frame-crc"))
        last = e
    if last < len(d):
        regs.append((last, len(d), "#trailing"))
    return regs


def flac_fix(orig, mut, regs, pos, n):
    """Make the frame checksums valid again. A window on a checksum keeps its marker."""
    out = bytearray(mut)
    meta, end = flac_meta(orig)
    fr = flac_frames(orig, end)
    for s, e, hl in fr:
        if e <= pos or s >= pos + n:
            continue
        if not (pos >= s + hl - 1 and pos + n > s + hl - 1 and pos < s + hl) and pos < s + hl and pos + n > s:
            if not (pos + n > s + hl - 1):
                out[s + hl - 1] = crc8(bytes(out[s:s + hl - 1]))
        if pos + n <= e - 2:
            out[e - 2:e] = struct.pack(">H", crc16(bytes(out[s:e - 2])))
    return bytes(out)


def flac_inserts(d):
    meta, end = flac_meta(d)
    blk = lambda t, M: bytes([t]) + len(M).to_bytes(3, "big") + M

    def add_block(M, at, gen, was_last_block):
        mid = gen(M)
        if was_last_block:
            # Clear the last flag on the previous last block and set it on the new one.
            out = bytearray(d[:at] + bytes([mid[0] | 0x80]) + mid[1:] + d[at:])
            s = meta[-1][1]
            out[s] &= 0x7f
            return bytes(out)
        return d[:at] + mid + d[at:]

    for i, (t, s, e, last) in enumerate(meta):
        for lab, gen in (("PADDING", lambda M: blk(1, M)), ("APPLICATION", lambda M: blk(2, b"ZZZZ" + M)), ("type9", lambda M: blk(9, M)),
                         ("VORBIS2", lambda M: blk(4, struct.pack("<I", len(M)) + M + struct.pack("<I", 0)))):
            yield lab, f"after:{FLAC_NAMES.get(t, t)}", (lambda M, e=e, gen=gen, last=last: add_block(M, e, gen, last))
    for t, s, e, last in meta:
        def grow(M, s=s, e=e):
            ln = int.from_bytes(d[s + 1:s + 4], "big") + len(M)
            return d[:s + 1] + ln.to_bytes(3, "big") + d[s + 4:e] + M + d[e:]
        yield "tail", f"tail:{FLAC_NAMES.get(t, t)}", grow
    fr = flac_frames(d, end)
    for (s, e, hl) in fr[:3] + fr[-1:]:
        yield "raw", "between-frames", (lambda M, s=s: d[:s] + M + d[s:])
    yield "raw_eof", "trailing", (lambda M: d + M)


# ----------------------------------------------------------------------------- MP3

BR = {(3, 1): [0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320],
      (2, 1): [0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160]}
SR = {3: [44100, 48000, 32000], 2: [22050, 24000, 16000], 0: [11025, 12000, 8000]}


def mp3_frame_len(h):
    if h[0] != 0xFF or (h[1] & 0xE0) != 0xE0:
        return 0
    ver = (h[1] >> 3) & 3
    layer = (h[1] >> 1) & 3
    if layer != 1 or ver == 1:
        return 0
    bi = h[2] >> 4
    si = (h[2] >> 2) & 3
    if bi in (0, 15) or si == 3:
        return 0
    br = BR[(3 if ver == 3 else 2, 1)][bi] * 1000
    sr = SR[ver][si]
    pad = (h[2] >> 1) & 1
    return ((144 if ver == 3 else 72) * br) // sr + pad


def mp3_regions(d):
    regs = []
    pos = 0
    if d[:3] == b"ID3":
        n = (d[6] << 21) | (d[7] << 14) | (d[8] << 7) | d[9]
        regs.append((0, 10 + n, "ID3v2"))
        pos = 10 + n
    end = len(d)
    if d[-128:-125] == b"TAG":
        end -= 128
    while pos + 4 <= end:
        ln = mp3_frame_len(d[pos:pos + 4])
        if not ln:
            regs.append((pos, end, "#unparsed"))
            pos = end
            break
        sidelen = (17 if d[pos + 3] >> 6 == 3 else 32) if (d[pos + 1] >> 3) & 3 == 3 else (9 if d[pos + 3] >> 6 == 3 else 17)
        is_info = d[pos + 4 + sidelen:pos + 8 + sidelen] in (b"Xing", b"Info")
        regs.append((pos, pos + 4, "frame-hdr"))
        regs.append((pos + 4, pos + ln, "xing-frame" if is_info else "frame-body"))
        pos += ln
    if end < len(d):
        regs.append((end, len(d), "ID3v1"))
    return regs


def mp3_inserts(d):
    regs = mp3_regions(d)
    frames = [r[0] for r in regs if r[2] == "frame-hdr"]
    def id3(M):
        fr = b"TXXX" + struct.pack(">I", len(M) + 2) + b"\0\0" + b"\0" + b"\0" + M
        n = len(fr)
        return b"ID3\x03\0\0" + bytes([(n >> 21) & 127, (n >> 14) & 127, (n >> 7) & 127, n & 127]) + fr

    def ape(M):
        item = struct.pack("<II", len(M), 0) + b"Comment\0" + M
        foot = b"APETAGEX" + struct.pack("<IIII", 2000, 32 + len(item), 1, 0) + b"\0" * 8
        return item + foot

    def lyr(M):
        body = b"LYRICSBEGIN" + b"IND0000200" + b"LYR" + b"%05d" % len(M) + M
        return body + b"%06d" % len(body) + b"LYRICS200"

    picks = frames[:2] + frames[len(frames) // 2:len(frames) // 2 + 1] + frames[-1:]
    for p in sorted(set(picks)):
        for lab, mk in (("raw", lambda M: M), ("id3mid", id3), ("ape", ape), ("lyr3", lyr)):
            yield lab, "between-frames", (lambda M, p=p, mk=mk: d[:p] + mk(M) + d[p:])
    # Before everything
    for lab, mk in (("raw", lambda M: M), ("id3", id3), ("ape", ape)):
        yield lab, "start", (lambda M, mk=mk: mk(M) + d)
    aud_end = len(d) - 128 if d[-128:-125] == b"TAG" else len(d)
    for lab, mk in (("raw", lambda M: M), ("id3v1", lambda M: b"TAG" + (M + b"\0" * 125)[:125]), ("ape", ape), ("lyr3", lyr), ("id3end", id3)):
        yield lab, "end", (lambda M, mk=mk: d[:aud_end] + mk(M) + d[aud_end:])
    yield "raw_eof", "trailing", (lambda M: d + M)
    if d[:3] == b"ID3":
        n = (d[6] << 21) | (d[7] << 14) | (d[8] << 7) | d[9]
        def grow(M):
            m = n + len(M)
            return d[:6] + bytes([(m >> 21) & 127, (m >> 14) & 127, (m >> 7) & 127, m & 127]) + d[10:10 + n] + M + d[10 + n:]
        yield "tail", "tail:ID3v2", grow


# ----------------------------------------------------------------------------- Ogg

OGG_TABLE = None


def ogg_crc(b):
    global OGG_TABLE
    if OGG_TABLE is None:
        OGG_TABLE = []
        for i in range(256):
            r = i << 24
            for _ in range(8):
                r = ((r << 1) ^ 0x04C11DB7) & 0xffffffff if r & 0x80000000 else (r << 1) & 0xffffffff
            OGG_TABLE.append(r)
    c = 0
    for x in b:
        c = ((c << 8) & 0xffffffff) ^ OGG_TABLE[((c >> 24) ^ x) & 255]
    return c


def ogg_pages(d):
    pos = 0
    out = []
    while pos + 27 <= len(d) and d[pos:pos + 4] == b"OggS":
        n = d[pos + 26]
        tbl = d[pos + 27:pos + 27 + n]
        body = sum(tbl)
        out.append((pos, 27 + n, pos + 27 + n + body))
        pos += 27 + n + body
    return out, pos


def ogg_regions(d):
    regs = []
    pages, end = ogg_pages(d)
    for i, (s, h, e) in enumerate(pages):
        if s == 0 or i < 3:
            lab = f"page{i}"
        else:
            lab = "page"
        regs.append((s, s + 18, lab + "#fields"))
        regs.append((s + 18, s + 22, lab + "#seq"))
        regs.append((s + 22, s + 26, lab + "#crc"))
        regs.append((s + 26, s + h, lab + "#segtab"))
        regs.append((s + h, e, lab + "#payload"))
    if end < len(d):
        regs.append((end, len(d), "#trailing"))
    return regs


def ogg_fix(orig, mut, regs, pos, n):
    out = bytearray(mut)
    pages, end = ogg_pages(orig)
    for s, h, e in pages:
        if e <= pos or s >= pos + n:
            continue
        if pos < s + 26 and pos + n > s + 22:
            continue  # the marker is in this page's CRC field
        out[s + 22:s + 26] = b"\0\0\0\0"
        out[s + 22:s + 26] = struct.pack("<I", ogg_crc(bytes(out[s:e])))
    return bytes(out)


def ogg_page(serial, seq, flags, payload, gran=0):
    segs = []
    n = len(payload)
    while n >= 255:
        segs.append(255)
        n -= 255
    segs.append(n)
    h = b"OggS\0" + bytes([flags]) + struct.pack("<qIII", gran, serial, seq, 0) + bytes([len(segs)]) + bytes(segs)
    crc = ogg_crc(h + payload)
    return h[:22] + struct.pack("<I", crc) + h[26:] + payload


def ogg_inserts(d):
    pages, end = ogg_pages(d)
    for i, (s, h, e) in enumerate(pages):
        if i == 0:
            continue
        yield "raw", "between-pages", (lambda M, s=s: d[:s] + M + d[s:])
        yield "page_other_serial", "between-pages", (lambda M, s=s: d[:s] + ogg_page(0x7a7a7a7a, 0, 0, M) + d[s:])
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "page_eof_other", "trailing", (lambda M: d + ogg_page(0x7a7a7a7a, 0, 2, M) + ogg_page(0x7a7a7a7a, 1, 4, b""))
    yield "raw_start", "start", (lambda M: M + d)

    # Grow the last packet in a page: lacing changes, CRC recomputed. Only pages with room.
    for i, (s, h, e) in enumerate(pages):
        n = d[s + 26]
        if n >= 255:
            continue

        def grow(M, s=s, h=h, e=e, n=n):
            seg = list(d[s + 27:s + 27 + n])
            # Add the marker as bytes of a packet that ends in this page: extend the last segment.
            if not seg or seg[-1] + len(M) >= 255:
                seg.append(len(M))
            else:
                seg[-1] += len(M)
            hdr = bytearray(d[s:s + 26]) + bytes([len(seg)]) + bytes(seg)
            page = bytes(hdr) + d[s + h:e] + M
            crc = ogg_crc(page[:22] + b"\0\0\0\0" + page[26:])
            page = page[:22] + struct.pack("<I", crc) + page[26:]
            return d[:s] + page + d[e:]

        lab = f"page{i}" if i < 3 else "page"
        yield "tail", f"tail:{lab}", grow


# ----------------------------------------------------------------------------- Matroska

M_MASTERS = {0x1A45DFA3, 0x18538067, 0x114D9B74, 0x4DBB, 0x1549A966, 0x1654AE6B, 0xAE, 0xE0, 0xE1, 0x6D80, 0x6240, 0x5034, 0x1C53BB6B,
             0xBB, 0xB7, 0x1F43B675, 0xA0, 0x1254C367, 0x7373, 0x63C0, 0x67C8, 0x1043A770, 0x45B9, 0xB6, 0x80, 0x1941A469, 0x61A7, 0x75A1,
             0xA6, 0x8F, 0xDB}
M_NAMES = {0x1A45DFA3: "EBML", 0x18538067: "Segment", 0x114D9B74: "SeekHead", 0x4DBB: "Seek", 0x1549A966: "Info", 0x1654AE6B: "Tracks", 0xAE: "TrackEntry",
           0xE0: "Video", 0xE1: "Audio", 0x1C53BB6B: "Cues", 0xBB: "CuePoint", 0xB7: "CueTrackPositions", 0x1F43B675: "Cluster", 0xA0: "BlockGroup",
           0x1254C367: "Tags", 0x7373: "Tag", 0x63C0: "Targets", 0x67C8: "SimpleTag", 0x1043A770: "Chapters", 0x45B9: "EditionEntry", 0xB6: "ChapterAtom",
           0x80: "ChapterDisplay", 0x1941A469: "Attachments", 0x61A7: "AttachedFile", 0xEC: "Void", 0xBF: "CRC-32", 0xA3: "SimpleBlock", 0xA1: "Block", 0x63A2: "CodecPrivate",
           0xE7: "Timecode", 0x53AB: "SeekID", 0x53AC: "SeekPosition", 0x4D80: "MuxingApp", 0x5741: "WritingApp", 0x7BA9: "Title", 0x73A4: "SegmentUID", 0x4489: "Duration", 0x2AD7B1: "TimecodeScale",
           0x45A3: "TagName", 0x4487: "TagString", 0x86: "CodecID", 0xD7: "TrackNumber", 0x73C5: "TrackUID", 0x83: "TrackType", 0x536E: "Name", 0x22B59C: "Language", 0x9C: "FlagLacing",
           0x23E383: "DefaultDuration", 0xB0: "PixelWidth", 0xBA: "PixelHeight", 0x54B0: "DisplayWidth", 0x54BA: "DisplayHeight", 0x9F: "Channels", 0xB5: "SamplingFreq", 0x6264: "BitDepth",
           0xB3: "CueTime", 0xF7: "CueTrack", 0xF1: "CueClusterPosition", 0xF0: "CueRelativePosition", 0x4282: "DocType", 0x4286: "EBMLVersion", 0x42F7: "EBMLReadVersion", 0x42F2: "EBMLMaxIDLength", 0x42F3: "EBMLMaxSizeLength", 0x4287: "DocTypeVersion", 0x4285: "DocTypeReadVersion"}


def ebml_id(d, p):
    b = d[p]
    n = 1
    mask = 0x80
    while n <= 4 and not (b & mask):
        mask >>= 1
        n += 1
    if n > 4 or p + n > len(d):
        return None, 0
    return int.from_bytes(d[p:p + n], "big"), n


def ebml_size(d, p):
    b = d[p]
    n = 1
    mask = 0x80
    while n <= 8 and not (b & mask):
        mask >>= 1
        n += 1
    if n > 8 or p + n > len(d):
        return None, 0
    v = b & (mask - 1)
    for i in range(1, n):
        v = (v << 8) | d[p + i]
    if v == (1 << (7 * n)) - 1:
        return -1, n
    return v, n


class El:
    __slots__ = ("id", "idlen", "start", "szlen", "dstart", "end", "path", "kids", "unknown")


def ebml_parse(d, start, end, path, depth=0):
    out = []
    p = start
    while p < end:
        i, il = ebml_id(d, p)
        if i is None:
            break
        if p + il >= end:
            break
        sz, sl = ebml_size(d, p + il)
        if sz is None:
            break
        e = El()
        e.id, e.idlen, e.start, e.szlen, e.dstart = i, il, p, sl, p + il + sl
        e.unknown = sz == -1
        e.end = end if sz == -1 else e.dstart + sz
        if e.end > end:
            break
        nm = M_NAMES.get(i, f"{i:X}")
        e.path = f"{path}/{nm}" if path else nm
        e.kids = ebml_parse(d, e.dstart, e.end, e.path, depth + 1)[0] if i in M_MASTERS and depth < 10 else None
        out.append(e)
        p = e.end
    return out, p


def ebml_regions(d):
    regs = []
    tree, end = ebml_parse(d, 0, len(d), "")

    def rec(nodes):
        for e in nodes:
            if e.kids is None:
                regs.append((e.start, e.dstart, e.path + "#hdr"))
                regs.append((e.dstart, e.end, e.path))
            else:
                regs.append((e.start, e.dstart, e.path + "#hdr"))
                rec(e.kids)

    rec(tree)
    if end < len(d):
        regs.append((end, len(d), "#trailing"))
    return regs


def ebml_enc_size(v, width):
    if v >= (1 << (7 * width)) - 1:
        width = 1
        while v >= (1 << (7 * width)) - 1:
            width += 1
    return ((1 << (7 * width)) | v).to_bytes(width, "big")


def ebml_ser(d, nodes, target, idx, ins):
    """Serialise, inserting ins at child index idx of element target (None for top)."""
    out = bytearray()
    for k, e in enumerate(nodes):
        if target is None and idx == k:
            out += ins
        out += ebml_ser_one(d, e, target, idx, ins)
    if target is None and idx == len(nodes):
        out += ins
    return bytes(out)


def ebml_ser_one(d, e, target, idx, ins):
    if e.kids is None:
        body = d[e.dstart:e.end]
        if e is target:
            body += ins
    else:
        b = bytearray()
        for k, c in enumerate(e.kids):
            if e is target and idx == k:
                b += ins
            b += ebml_ser_one(d, c, target, idx, ins)
        if e is target and idx == len(e.kids):
            b += ins
        body = bytes(b)
    hdr = d[e.start:e.start + e.idlen]
    if e.unknown:
        return hdr + d[e.start + e.idlen:e.dstart] + body
    return hdr + ebml_enc_size(len(body), e.szlen) + body


def ebml_fix_positions(out, orig, pos, delta):
    """Fix SeekPosition and CueClusterPosition. Both are relative to the Segment's data."""
    tree, _ = ebml_parse(out, 0, len(out), "")
    otree, _ = ebml_parse(orig, 0, len(orig), "")
    seg = next((e for e in tree if e.id == 0x18538067), None)
    oseg = next((e for e in otree if e.id == 0x18538067), None)
    if seg is None or oseg is None:
        return out
    rel = pos - oseg.dstart
    b = bytearray(out)

    def rec(nodes):
        for e in nodes:
            if e.id in (0x53AC, 0xF1):
                v = int.from_bytes(b[e.dstart:e.end], "big")
                if v >= rel:
                    nv = v + delta
                    w = e.end - e.dstart
                    if nv < 1 << (8 * w):
                        b[e.dstart:e.end] = nv.to_bytes(w, "big")
            if e.kids:
                rec(e.kids)

    rec(seg.kids or [])
    return bytes(b)


def ebml_inserts(d):
    tree, end = ebml_parse(d, 0, len(d), "")
    el = lambda idb, M: idb + ebml_enc_size(len(M), 1) + M

    def mk_insert(M, target, idx, ins):
        pos = (target.kids[idx].start if idx < len(target.kids) else target.end) if target is not None else 0
        out = ebml_ser(d, tree, target, idx, ins)
        return ebml_fix_positions(out, d, pos, len(out) - len(d))

    def rec(nodes):
        for e in nodes:
            if e.kids is not None and e.id != 0x1A45DFA3:
                for idx in range(len(e.kids) + 1):
                    for lab, idb in (("Void", b"\xec"), ("unknownID", b"\x4e\x4e"), ("CRC32tail", b"\xbf")):
                        yield lab, f"insert:{e.path}", (lambda M, e=e, idx=idx, idb=idb: mk_insert(M, e, idx, el(idb, M)))
                yield from rec(e.kids)
            if e.kids is None and e.id not in (0xEC,):
                yield "tail", f"tail:{e.path}", (lambda M, e=e: mk_insert_tail(M, e))

    def mk_insert_tail(M, e):
        out = ebml_ser(d, tree, e, 0, M)
        return ebml_fix_positions(out, d, e.end, len(out) - len(d))

    yield from rec(tree)
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "void_eof", "trailing", (lambda M: d + el(b"\xec", M))


# ----------------------------------------------------------------------------- PDF

OBJ_RE = re.compile(rb"(\d+) (\d+) obj\b")


def pdf_regions(d):
    regs = []
    pos = 0
    hdr_end = d.find(b"\n", d.find(b"\n") + 1) + 1
    regs.append((0, hdr_end, "header"))
    pos = hdr_end
    for m in OBJ_RE.finditer(d):
        s = m.start()
        if s < pos:
            continue
        if s > pos:
            regs.append((pos, s, "#gap"))
        e = d.find(b"endobj", s)
        if e < 0:
            e = len(d) - 6
        e += 6
        body = d[s:e]
        st = body.find(b"stream")
        kind = "obj"
        for lab, rx in (("Info", rb"/Author|/Producer|/Creator"), ("Catalog", rb"/Type\s*/Catalog"), ("Pages", rb"/Type\s*/Pages"), ("Page", rb"/Type\s*/Page\b"),
                        ("Font", rb"/Type\s*/Font"), ("XMP", rb"/Type\s*/Metadata"), ("ObjStm", rb"/Type\s*/ObjStm"), ("XRefStm", rb"/Type\s*/XRef")):
            if re.search(rx, body[:400]):
                kind = lab
                break
        if st >= 0 and body[st - 3:st] != b"end":
            ds = s + st + 6
            if d[ds:ds + 2] == b"\r\n":
                ds += 2
            elif d[ds:ds + 1] == b"\n":
                ds += 1
            ee = d.find(b"endstream", ds)
            regs.append((s, ds, f"{kind}#dict"))
            regs.append((ds, ee, f"{kind}#stream"))
            regs.append((ee, e, f"{kind}#end"))
        else:
            regs.append((s, e, f"{kind}#dict"))
        pos = e
    x = d.find(b"xref", pos) if d.find(b"xref", pos) >= 0 else None
    if x is not None:
        if x > pos:
            regs.append((pos, x, "#gap"))
        t = d.find(b"trailer", x)
        if t > 0:
            regs.append((x, t, "xref"))
            sx = d.find(b"startxref", t)
            regs.append((t, sx, "trailer"))
            pos = sx
        else:
            regs.append((x, len(d), "tail"))
            pos = len(d)
    if pos < len(d):
        regs.append((pos, len(d), "startxref+eof"))
    return regs


def pdf_fix_xref(d, pos, delta):
    """Shift the classic xref table and startxref after inserting delta bytes at pos."""
    m = list(re.finditer(rb"startxref\s+(\d+)", d))
    if not m:
        return d
    m = m[-1]
    sx = int(m.group(1))
    out = bytearray(d)
    # In the shifted file the table sits at sx (+delta if after pos)
    tbl = sx + delta if sx >= pos else sx
    if out[tbl:tbl + 4] != b"xref":
        return d
    p = tbl + 4
    while p < len(out) and out[p] in b"\r\n ":
        p += 1
    while True:
        lm = re.match(rb"(\d+) (\d+)\s+", bytes(out[p:p + 40]))
        if not lm:
            break
        first, cnt = int(lm.group(1)), int(lm.group(2))
        p += lm.end()
        for i in range(cnt):
            ent = bytes(out[p:p + 20])
            if ent[17:18] == b"n":
                v = int(ent[:10])
                if v >= pos:
                    out[p:p + 10] = b"%010d" % (v + delta)
            p += 20
    # trailer /Prev is not used in these samples
    s = bytes(out)
    # recompute location of startxref value in the shifted file
    m2 = list(re.finditer(rb"startxref\s+(\d+)", s))[-1]
    return s[:m2.start(1)] + b"%d" % tbl + s[m2.end(1):]


def pdf_inserts(d):
    classic = b"startxref" in d and re.search(rb"\nxref\s", d) is not None

    def ins(M, pos, mk):
        b = mk(M)
        out = d[:pos] + b + d[pos:]
        return pdf_fix_xref(out, pos, len(b)) if classic else out

    regs = pdf_regions(d)
    bounds = sorted({s for s, e, p in regs if p.endswith("#dict") or p == "xref"})
    if not classic:
        # Offsets cannot be kept valid here without rewriting a compressed table.
        yield "raw_eof", "trailing", (lambda M: d + M)
        yield "comment_eof", "trailing", (lambda M: d + b"%" + M + b"\n")
        return
    for pos in bounds:
        for lab, mk in (("comment", lambda M: b"%" + M + b"\n"), ("raw_ws", lambda M: b" " + M + b" "),
                        ("unref_obj", lambda M: b"999 0 obj\n(" + M + b")\nendobj\n"),
                        ("unref_stream", lambda M: b"998 0 obj\n<< /Length %d >>\nstream\n" % len(M) + M + b"\nendstream\nendobj\n")):
            yield lab, "boundary", (lambda M, pos=pos, mk=mk: ins(M, pos, mk))
    # Inside each dictionary, at its opening <<: a private key, and comments and whitespace.
    for m in re.finditer(rb"\d+ \d+ obj\s*<<", d):
        pos = m.end()
        path = next((p for s, e, p in regs if s <= pos < e), "?")
        for lab, mk in (("key_private", lambda M: b"/ZzPriv (" + M + b") "), ("comment_in_dict", lambda M: b"%" + M + b"\n"),
                        ("key_hex", lambda M: b"/ZzHex <" + M.hex().encode() + b"> ")):
            yield lab, f"dict:{path}", (lambda M, pos=pos, mk=mk: ins(M, pos, mk))
    # Bytes after a stream's data and before endstream, inside and beyond /Length
    for m in re.finditer(rb"endstream", d):
        pos = m.start()
        path = next((p for s, e, p in regs if s <= pos < e), "?")
        yield "stream_tail", f"stream:{path}", (lambda M, pos=pos: ins(M, pos, lambda M: M))
    for m in re.finditer(rb"endobj\s+", d):
        pos = m.end()
        yield "after_endobj", "boundary", (lambda M, pos=pos: ins(M, pos, lambda M: b"%" + M + b"\n"))
    tr = d.find(b"trailer")
    if tr > 0:
        q = d.find(b"<<", tr) + 2
        yield "key_trailer", "trailer", (lambda M: ins(M, q, lambda M: b"/ZzPriv (" + M + b") "))
        yield "comment_trailer", "trailer", (lambda M: ins(M, q, lambda M: b"%" + M + b"\n"))
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "comment_eof", "trailing", (lambda M: d + b"%" + M + b"\n")


# ----------------------------------------------------------------------------- SVG

SVG_TAG = re.compile(rb"<[^>]*>", re.S)


def svg_regions(d):
    regs = []
    pos = 0
    stack = []
    for m in SVG_TAG.finditer(d):
        s, e = m.span()
        if s > pos:
            regs.append((pos, s, "/".join(stack[-2:]) + "#text"))
        t = m.group(0)
        if t.startswith(b"<!--"):
            name = "comment"
        elif t.startswith(b"<?"):
            name = "pi"
        elif t.startswith(b"</"):
            name = t[2:-1].decode("latin1").strip()
            if stack:
                stack.pop()
            name = "/" + name
        else:
            name = re.match(rb"<([^\s/>]+)", t).group(1).decode("latin1")
            if not t.endswith(b"/>"):
                stack.append(name)
        regs.append((s, e, name))
        pos = e
    if pos < len(d):
        regs.append((pos, len(d), "#trailing"))
    return regs


def svg_inserts(d):
    tags = list(SVG_TAG.finditer(d))
    for m in tags:
        s, e = m.span()
        name = re.match(rb"<[/?!]*([^\s/>]*)", m.group(0)).group(1).decode("latin1")
        for lab, mk in (("comment", lambda M: b"<!--" + M + b"-->"), ("pi", lambda M: b"<?zz " + M + b"?>"),
                        ("cdata_text", lambda M: b"<![CDATA[" + M + b"]]>"), ("text", lambda M: M),
                        ("foreign_el", lambda M: b'<zz:a xmlns:zz="urn:zz">' + M + b"</zz:a>"),
                        ("foreign_attr_el", lambda M: b'<zz:a xmlns:zz="urn:zz" v="' + M + b'"/>'),
                        ("metadata", lambda M: b"<metadata>" + M + b"</metadata>"),
                        ("unknown_svg_el", lambda M: b"<zzq>" + M + b"</zzq>")):
            yield lab, f"after:{name}", (lambda M, e=e, mk=mk: d[:e] + mk(M) + d[e:])
        t = m.group(0)
        if t.startswith((b"<!--", b"<?", b"</")):
            continue
        at = s + 1 + len(name)
        for lab, mk in (("attr_data", lambda M: b' data-zz="' + M + b'"'), ("attr_unknown", lambda M: b' zzattr="' + M + b'"'),
                        ("attr_foreign", lambda M: b' xmlns:zy="urn:zy" zy:a="' + M + b'"'), ("attr_inkscape", lambda M: b' inkscape:zz="' + M + b'"'),
                        ("attr_ns_decl", lambda M: b' xmlns:zw="urn:' + M + b'"')):
            yield lab, f"in:{name}", (lambda M, at=at, mk=mk: d[:at] + mk(M) + d[at:])
    yield "raw_eof", "trailing", (lambda M: d + M)
    yield "comment_eof", "trailing", (lambda M: d + b"<!--" + M + b"-->")
    yield "doctype", "prolog", (lambda M: d[:d.find(b"<svg")] + b'<!DOCTYPE svg [<!ENTITY zz "' + M + b'">]>' + d[d.find(b"<svg"):])
    yield "bom_ws", "prolog", (lambda M: d[:d.find(b"?>") + 2] + b"<!--" + M + b"-->" + d[d.find(b"?>") + 2:])


FORMATS = {
    "iso": (iso_regions, None, iso_inserts),
    "riff": (riff_regions, None, riff_inserts),
    "webp": (lambda d: riff_regions(d, webp=True), None, lambda d: riff_inserts(d, webp=True)),
    "avi": (riff_regions, None, lambda d: riff_inserts(d, avi=True)),
    "aiff": (lambda d: riff_regions(d, be=True), None, lambda d: riff_inserts(d, be=True)),
    "jpeg": (jpeg_regions, None, jpeg_inserts),
    "png": (png_regions, png_fix, png_inserts),
    "gif": (gif_regions, None, gif_inserts),
    "tiff": (tiff_regions, None, tiff_inserts),
    "flac": (flac_regions, flac_fix, flac_inserts),
    "mp3": (mp3_regions, None, mp3_inserts),
    "ogg": (ogg_regions, ogg_fix, ogg_inserts),
    "ebml": (ebml_regions, None, ebml_inserts),
    "pdf": (pdf_regions, None, pdf_inserts),
    "svg": (svg_regions, None, svg_inserts),
}
