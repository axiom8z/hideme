#!/usr/bin/env python
"""Office slack sweep: overwrite and insert in the decompressed parts, rezip, keep what a
library reads identically, then see what HideMe does. ZIP bytes themselves are rewritten by
HideMe, so the marker is looked for in the decompressed parts of the output, and in its raw bytes.
"""
import argparse, hashlib, io, json, logging, multiprocessing as mp, os, re, shutil, subprocess, time, warnings, zipfile
from collections import Counter

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(HERE, "work")
EXE = ".exe" if os.name == "nt" else ""


def rezip(parts, order, comment=b"", entry_comment=None, extra=None):
    b = io.BytesIO()
    with zipfile.ZipFile(b, "w", zipfile.ZIP_DEFLATED) as z:
        for n in order:
            zi = zipfile.ZipInfo(n, date_time=(1980, 1, 1, 0, 0, 0))
            zi.compress_type = zipfile.ZIP_STORED if n == "mimetype" else zipfile.ZIP_DEFLATED
            if entry_comment and n == entry_comment[0]:
                zi.comment = entry_comment[1]
            if extra and n == extra[0]:
                zi.extra = extra[1]
            z.writestr(zi, parts[n])
        z.comment = comment
    return b.getvalue()


def read_parts(data):
    with zipfile.ZipFile(io.BytesIO(data)) as z:
        order = [i.filename for i in z.infolist()]
        return {n: z.read(n) for n in order}, order


def docx_text(data):
    import docx
    d = docx.Document(io.BytesIO(data))
    out = [p.text for p in d.paragraphs]
    for t in d.tables:
        for r in t.rows:
            out += [c.text for c in r.cells]
    for s in d.sections:
        out += [p.text for p in s.header.paragraphs] + [p.text for p in s.footer.paragraphs]
    return out


def xlsx_text(data):
    import openpyxl
    w = openpyxl.load_workbook(io.BytesIO(data))
    out = list(w.sheetnames)
    for ws in w.worksheets:
        for row in ws.iter_rows():
            out += [(c.coordinate, c.value) for c in row if c.value is not None]
    return out


def pptx_text(data):
    import pptx
    p = pptx.Presentation(io.BytesIO(data))
    out = []
    for s in p.slides:
        for sh in s.shapes:
            if sh.has_text_frame:
                out.append(sh.text_frame.text)
        if s.has_notes_slide:
            out.append(s.notes_slide.notes_text_frame.text)
    return out


def odt_text(data):
    from odf.opendocument import load
    from odf import teletype
    d = load(io.BytesIO(data))
    return [teletype.extractText(d.text)]


READERS = {"docx": docx_text, "xlsx": xlsx_text, "pptx": pptx_text, "odt": odt_text}


def digest(kind, data):
    warnings.simplefilter("ignore")
    logging.disable(logging.CRITICAL)
    try:
        return hashlib.md5(repr(READERS[kind](data)).encode("utf-8", "replace")).hexdigest()
    except Exception:
        return None


G = {}


def init_worker(kind, sample, base):
    G.update(kind=kind, sample=sample, base=base)
    G["parts"], G["order"] = read_parts(sample)


def marker(i, L):
    if L >= 8:
        return b"Q%06XZ" % i
    al = b"abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
    n = len(al)
    return bytes([al[i % n], al[(i // n) % n], al[(i // n // n) % n], 0x7E])


def try_over(args):
    part, pos, L, idx = args
    parts = G["parts"]
    M = marker(idx, L)
    if parts[part][pos:pos + L] == M:
        return None
    p2 = dict(parts)
    p2[part] = parts[part][:pos] + M + parts[part][pos + L:]
    data = rezip(p2, G["order"])
    if digest(G["kind"], data) == G["base"]:
        return (part, pos, L, M, data)
    return None


def try_ins(args):
    label, path, M, data = args
    if digest(G["kind"], data) == G["base"]:
        return (label, path, M, data)
    return None


TAG = re.compile(rb"<[^>]+>", re.S)


def locate(b, pos):
    """Name what a position in an XML part falls in."""
    lt = b.rfind(b"<", 0, pos + 1)
    gt = b.rfind(b">", 0, pos + 1)
    if lt > gt:
        m = re.match(rb"<[/?!]*([^\s/>]*)", b[lt:lt + 80])
        name = m.group(1).decode("latin1") if m else "?"
        head = b[lt:pos]
        am = list(re.finditer(rb'([\w:.-]+)\s*=\s*"[^"]*$', head))
        if am:
            return f"{name}@{am[-1].group(1).decode('latin1')}"
        return f"<{name}>"
    # text
    m = re.match(rb"<([^\s/>]*)", b[lt:lt + 80]) if lt >= 0 else None
    return f"text:{m.group(1).decode('latin1') if m else '?'}"


def inserts(parts, order, kind):
    """Yields (label, path, make(marker) -> file bytes)."""
    xmlparts = [n for n in order if n.endswith((".xml", ".rels"))]
    for n in xmlparts:
        b = parts[n]
        tags = [m for m in TAG.finditer(b) if not m.group(0).startswith((b"<?", b"<!"))]
        step = max(1, len(tags) // 24)
        for m in tags[::step]:
            s, e = m.span()
            t = m.group(0)
            name = re.match(rb"<[/]*([^\s/>]*)", t).group(1).decode("latin1")
            after = [("comment", lambda M: b"<!--" + M + b"-->"), ("pi", lambda M: b"<?zz " + M + b"?>"),
                     ("foreign_el", lambda M: b'<zz:a xmlns:zz="urn:zz">' + M + b"</zz:a>"),
                     ("foreign_empty", lambda M: b'<zz:a xmlns:zz="urn:zz" v="' + M + b'"/>'),
                     ("text", lambda M: M), ("cdata", lambda M: b"<![CDATA[" + M + b"]]>")]
            own = name.split(":")[0].encode() + b":" if ":" in name else b""
            after = after + [("own_ns_el", lambda M, own=own: b"<" + own + b"zzq>" + M + b"</" + own + b"zzq>")]
            for lab, mk in after:
                yield lab, f"{n}|after:{name}", (lambda M, n=n, e=e, mk=mk: rz(parts, order, n, parts[n][:e] + mk(M) + parts[n][e:]))
            if not t.startswith(b"</"):
                at = s + 1 + len(name)
                for lab, mk in (("attr_foreign", lambda M: b' xmlns:zy="urn:zy" zy:a="' + M + b'"'), ("attr_plain", lambda M: b' zzplain="' + M + b'"'),
                                ("attr_nsdecl", lambda M: b' xmlns:zw="urn:' + M + b'"'), ("own_ns_attr", lambda M, own=own: b" " + own + b'zzattr="' + M + b'"')):
                    yield lab, f"{n}|in:{name}", (lambda M, n=n, at=at, mk=mk: rz(parts, order, n, parts[n][:at] + mk(M) + parts[n][at:]))
        # After the root's end, and before it: outside the document element
        yield "after_root_comment", f"{n}|end", (lambda M, n=n: rz(parts, order, n, parts[n] + b"<!--" + M + b"-->"))
        yield "after_root_ws_pi", f"{n}|end", (lambda M, n=n: rz(parts, order, n, parts[n] + b"<?zz " + M + b"?>"))
        if b.startswith(b"<?xml"):
            q = b.find(b"?>") + 2
            yield "prolog_comment", f"{n}|prolog", (lambda M, n=n, q=q: rz(parts, order, n, parts[n][:q] + b"<!--" + M + b"-->" + parts[n][q:]))
            yield "doctype", f"{n}|prolog", (lambda M, n=n, q=q: rz(parts, order, n, parts[n][:q] + b'<!DOCTYPE a [<!ENTITY zz "' + M + b'">]>' + parts[n][q:]))
    # Relationships
    for n in [x for x in order if x.endswith(".rels")]:
        end = parts[n].rfind(b"</Relationships>")
        for lab, mk in (("rel_internal", lambda M: b'<Relationship Id="rIdZZ" Type="http://zz/' + M + b'" Target="zz.xml"/>'),
                        ("rel_external", lambda M: b'<Relationship Id="rIdZZ" Type="http://zz/x" Target="http://' + M + b'/" TargetMode="External"/>'),
                        ("rel_hyperlink", lambda M: b'<Relationship Id="rIdZZ" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="file:///C:/' + M + b'" TargetMode="External"/>')):
            yield lab, f"{n}|rels", (lambda M, n=n, end=end, mk=mk: rz(parts, order, n, parts[n][:end] + mk(M) + parts[n][end:]))
    # New parts
    def newpart(M, name, body):
        p2 = dict(parts)
        nm = name.replace("MARK", M.decode())
        p2[nm] = body.replace(b"MARK", M)
        return rezip(p2, order + [nm])
    for name, body in (("customXml/zz.xml", b"<a>MARK</a>"), ("zz/MARK.bin", b"x"), ("zz/zz.bin", b"MARK"), ("docProps/zz.xml", b"<a>MARK</a>"), ("customXml/item9.xml", b'<?xml version="1.0"?><b>MARK</b>'),
                       ("word/zz.xml", b"<a>MARK</a>"), ("Thumbnails/zz.png", b"MARK"), ("meta.xml.bak", b"MARK"), ("_rels/zz.rels", b"<Relationships>MARK</Relationships>")):
        yield "new_part", name, (lambda M, name=name, body=body: newpart(M, name, body))
    # ZIP level
    yield "zip_comment", "zip", (lambda M: rezip(parts, order, comment=M))
    yield "entry_comment", "zip", (lambda M: rezip(parts, order, entry_comment=(order[0], M)))
    yield "entry_extra", "zip", (lambda M: rezip(parts, order, extra=(order[0], b"\x99\x99" + len(M).to_bytes(2, "little") + M)))
    yield "trailing", "zip", (lambda M: rezip(parts, order) + M)
    yield "prefix", "zip", (lambda M: M + rezip(parts, order))


def rz(parts, order, n, new):
    p2 = dict(parts)
    p2[n] = new
    return rezip(p2, order)


def output_has(path, M):
    with open(path, "rb") as f:
        raw = f.read()
    if M in raw:
        return True, "raw"
    try:
        parts, _ = read_parts(raw)
    except Exception:
        return False, ""
    for n, b in parts.items():
        if M in b or M in n.encode("utf-8", "replace"):
            return True, n
    return False, ""


def run_kind(a, kind, name):
    with open(os.path.join(a.samples, name), "rb") as f:
        sample = f.read()
    work = os.path.join(a.work, name)
    shutil.rmtree(work, ignore_errors=True)
    kept_dir = os.path.join(work, "kept")
    os.makedirs(kept_dir)
    base = digest(kind, sample)
    if base is None:
        print(name, "the reader cannot read the sample")
        return
    parts, order = read_parts(sample)
    # Rezipped baseline, so that nothing here is about the ZIP layout
    base_zip = rezip(parts, order)
    if digest(kind, base_zip) != base:
        print(name, "rezip changed the reading")
        return
    base_path = os.path.join(work, "base" + os.path.splitext(name)[1])
    with open(base_path, "wb") as f:
        f.write(base_zip)
    r = subprocess.run([a.bin, "clean", "--json", "-o", os.path.join(work, "bo"), "--force", base_path], capture_output=True)
    br = json.loads(r.stdout.decode("utf-8", "replace"))["results"][0]
    if br["status"] != "cleaned":
        print(name, "BASELINE REFUSED", br.get("error_kind"))
        return
    base_warn = set(br.get("warnings", []))
    jobs, stride = [], {}
    idx = 0
    for n in order:
        if not n.endswith((".xml", ".rels")) and n != "mimetype":
            continue
        size = len(parts[n])
        st = max(1, size // a.max_per_part)
        stride[n] = st
        for L in a.sizes:
            for pos in range(0, max(0, size - L + 1), st):
                jobs.append((n, pos, L, idx))
                idx += 1
    ins = []
    for i, (label, path, mk) in enumerate(inserts(parts, order, kind)):
        M = b"I%06XZ" % i
        try:
            ins.append((label, path, M, mk(M)))
        except Exception as e:
            print("insert failed", label, path, e)
    print(f"{name}: {len(jobs)} overwrites, {len(ins)} inserts, largest stride {max(stride.values())}", flush=True)
    rows = []
    t0 = time.time()
    with mp.Pool(a.jobs, init_worker, (kind, base_zip, base)) as pool:
        for res in pool.imap_unordered(try_over, jobs, chunksize=8):
            if res is None:
                continue
            part, pos, L, M, data = res
            fn = f"o{L}_{len(rows)}{os.path.splitext(name)[1]}"
            with open(os.path.join(kept_dir, fn), "wb") as f:
                f.write(data)
            rows.append({"file": fn, "mode": f"over{L}", "pos": pos, "marker": M.decode(), "part": part, "path": part + "|" + locate(parts[part], pos)})
        for res in pool.imap_unordered(try_ins, ins, chunksize=4):
            if res is None:
                continue
            label, path, M, data = res
            fn = f"i_{M.decode()}{os.path.splitext(name)[1]}"
            with open(os.path.join(kept_dir, fn), "wb") as f:
                f.write(data)
            rows.append({"file": fn, "mode": "insert", "pos": -1, "marker": M.decode(), "part": "", "path": path + "|" + label})
    t1 = time.time()
    results = {}
    if rows:
        r = subprocess.run([a.bin, "clean", "--json", "--jobs", str(a.jobs), "-r", "-o", os.path.join(work, "out"), "--force", kept_dir], capture_output=True)
        doc = json.loads(r.stdout.decode("utf-8", "replace"))
        for x in doc["results"]:
            results[os.path.basename(x["path"].replace("\\", "/"))] = x
    for row in rows:
        x = results.get(row["file"])
        if x is None:
            row["class"] = "NORESULT"
            continue
        if x["status"] != "cleaned":
            row["class"] = "REFUSED"
            row["kind"] = x.get("error_kind")
            continue
        has, where = output_has(x["output"], row["marker"].encode())
        new_warn = [w for w in x.get("warnings", []) if w not in base_warn]
        if not has:
            row["class"] = "REMOVED"
        elif new_warn:
            row["class"] = "WARNED"
            row["warn"] = new_warn[:2]
        else:
            row["class"] = "SILENT"
            retained = [f["field"] for f in x["findings"] if f["action"] == "retain"]
            if retained:
                row["retained"] = retained[:3]
            row["where"] = where
    rows.sort(key=lambda r: (r["path"], r["mode"], r["pos"]))
    os.makedirs(a.out, exist_ok=True)
    doc = {"name": name, "size": len(sample), "tried": {"over": len(jobs), "insert": len(ins)}, "stride": max(stride.values()), "stride_per_part": stride,
           "rows": rows, "base_warn": sorted(base_warn), "base_same_decode": digest(kind, open(br["output"], "rb").read()) == base,
           "secs": [round(t1 - t0, 1), round(time.time() - t1, 1)]}
    with open(os.path.join(a.out, name + ".json"), "w") as f:
        json.dump(doc, f, indent=0)
    shutil.rmtree(work, ignore_errors=True)
    print(f"== {name} tried={doc['tried']} kept={len(rows)} {dict(Counter(r['class'] for r in rows))} base_decode_ok={doc['base_same_decode']} {doc['secs']}", flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--bin", default=os.path.join(WORK, "hideme" + EXE), help="the binary under test")
    ap.add_argument("--samples", default=os.path.join(WORK, "samples"), help="what mksamples.py wrote")
    ap.add_argument("--work", default=os.path.join(WORK, "scratch_office"), help="temporary files, removed as each sample ends")
    ap.add_argument("--out", default=os.path.join(WORK, "results_office"), help="one JSON file per sample")
    ap.add_argument("--only", default="docx,xlsx,pptx,odt")
    ap.add_argument("--sizes", default="8,4")
    ap.add_argument("--max-per-part", type=int, default=1500, help="a part is overwritten every (its size // this) bytes, for each marker length; smaller parts at every byte")
    ap.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) - 2))
    a = ap.parse_args()
    a.sizes = [int(x) for x in a.sizes.split(",")]
    for kind in a.only.split(","):
        run_kind(a, kind, f"{kind}.{kind}")


if __name__ == "__main__":
    mp.freeze_support()
    main()
