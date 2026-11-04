#!/usr/bin/env python
"""Slack-space sweep for media, image and PDF samples.

Overwrites every byte position of a sample with a marker, and inserts the marker at
structural positions. A mutated file counts as a real hiding spot only if an independent
reader decodes it identically to the original. The real binary then cleans each such file,
and the result is classified as REMOVED, REFUSED, WARNED or SILENT. See README.md and
docs/SWEEP.md for what those mean.
"""
import argparse
import hashlib
import io
import json
import logging
import multiprocessing as mp
import os
import re
import shutil
import subprocess
import tempfile
import time
import warnings
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict

import imageio_ffmpeg

import structs

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(HERE, "work")
EXE = ".exe" if os.name == "nt" else ""
FFMPEG = imageio_ffmpeg.get_ffmpeg_exe()

# sample name -> (structure parser, reader)
SAMPLES = {
    "jpeg_baseline.jpg": ("jpeg", "pillow"), "jpeg_progressive.jpg": ("jpeg", "pillow"), "jpeg_icc_dri.jpg": ("jpeg", "pillow"),
    "png_rgb.png": ("png", "pillow"), "png_palette.png": ("png", "pillow"), "png_icc.png": ("png", "pillow"), "apng.png": ("png", "pillow"),
    "webp_lossy.webp": ("webp", "pillow"), "webp_lossless.webp": ("webp", "pillow"), "webp_simple.webp": ("webp", "pillow"), "webp_anim.webp": ("webp", "pillow"),
    "gif_anim.gif": ("gif", "pillow"), "gif_still.gif": ("gif", "pillow"),
    "tiff_raw.tif": ("tiff", "pillow"), "tiff_lzw.tif": ("tiff", "pillow"), "tiff_deflate.tif": ("tiff", "pillow"), "tiff_jpeg.tif": ("tiff", "pillow"), "tiff_multi.tif": ("tiff", "pillow"),
    "heic.heic": ("iso", "heif"), "heic_icc.heic": ("iso", "heif"), "heic_multi.heic": ("iso", "heif"), "avif.avif": ("iso", "pillow"),
    "mp4_plain.mp4": ("iso", "ff"), "mp4_mdta.mp4": ("iso", "ff"), "mp4_faststart.mp4": ("iso", "ff"), "mp4_frag.mp4": ("iso", "ff"), "mp4_ismv.mp4": ("iso", "ff"),
    "mov_plain.mov": ("iso", "ff"), "mov_pcm.mov": ("iso", "ff"), "m4a.m4a": ("iso", "ff"),
    "mkv.mkv": ("ebml", "ff"), "mkv_flac.mkv": ("ebml", "ff"), "webm_vp8.webm": ("ebml", "ff"), "webm_vp9.webm": ("ebml", "ff"),
    "avi_mpeg4.avi": ("avi", "ff"), "avi_mjpeg.avi": ("avi", "ff"),
    "mp3.mp3": ("mp3", "ff"), "flac.flac": ("flac", "ff"), "vorbis.ogg": ("ogg", "ff"), "opus.opus": ("ogg", "ff"),
    "wav.wav": ("riff", "ff"), "aiff.aiff": ("aiff", "ff"),
    "pdf_reportlab.pdf": ("pdf", "pdf"), "pdf_plainxmp.pdf": ("pdf", "pdf"), "pdf_objstm.pdf": ("pdf", "pdf"),
    "svg.svg": ("svg", "svg"),
}

SVG_NS = "{http://www.w3.org/2000/svg}"
TEXTUAL = {SVG_NS + t for t in ("text", "tspan", "textPath", "style", "script", "a", "switch", "foreignObject")}
SKIP_ELS = {SVG_NS + t for t in ("metadata", "title", "desc", "zzq")}


def svg_digest(data):
    """What an SVG renderer uses: the tree of SVG elements, their attributes and their text."""
    try:
        root = ET.fromstring(data)
    except Exception:
        return None
    if root.tag != SVG_NS + "svg":
        return None
    out = []

    def rec(e, depth):
        if not isinstance(e.tag, str) or e.tag in SKIP_ELS or not e.tag.startswith(SVG_NS):
            return
        attrs = sorted((k, v) for k, v in e.attrib.items() if (not k.startswith("{") or "xlink" in k) and not k.startswith("data-") and k != "zzattr")
        out.append((depth, e.tag, attrs, (e.text or "").strip() if e.tag in TEXTUAL else ""))
        for c in e:
            rec(c, depth + 1)

    rec(root, 0)
    return hashlib.md5(repr(out).encode()).hexdigest()


def pillow_digest(data):
    from PIL import Image
    warnings.simplefilter("ignore")
    Image.MAX_IMAGE_PIXELS = 4_000_000
    h = hashlib.md5()
    try:
        im = Image.open(io.BytesIO(data))
        n = getattr(im, "n_frames", 1)
        if n > 64:
            return None
        for i in range(n):
            im.seek(i)
            im.load()
            h.update(repr((im.mode, im.size, im.info.get("duration"), im.info.get("disposal"))).encode())
            h.update(im.tobytes())
            # A palette is decoded content: indices alone would hide a changed colour.
            if im.mode in ("P", "PA", "1", "L", "LA"):
                h.update(im.convert("RGBA").tobytes())
    except Exception:
        return None
    return f"{n}:{h.hexdigest()}"


def heif_digest(data):
    import pillow_heif
    warnings.simplefilter("ignore")
    try:
        hf = pillow_heif.open_heif(io.BytesIO(data), convert_hdr_to_8bit=False)
        h = hashlib.md5()
        for im in hf:
            h.update(repr((im.mode, im.size)).encode())
            h.update(bytes(im.data))
        return f"{len(hf)}:{h.hexdigest()}"
    except Exception:
        return None


def ff_digest(data, ext, tmp):
    p = os.path.join(tmp, f"w{os.getpid()}{ext}")
    with open(p, "wb") as f:
        f.write(data)
    try:
        r = subprocess.run([FFMPEG, "-v", "quiet", "-nostdin", "-i", p, "-map", "0:v?", "-map", "0:a?", "-f", "md5", "-"], capture_output=True, timeout=30)
    except subprocess.TimeoutExpired:
        return None
    m = re.search(rb"MD5=([0-9a-f]{32})", r.stdout)
    return m.group(1).decode() if m and r.returncode == 0 else None


def pdf_digest(data):
    import pypdf
    logging.disable(logging.CRITICAL)
    warnings.simplefilter("ignore")
    try:
        r = pypdf.PdfReader(io.BytesIO(data), strict=False)
        if r.is_encrypted:
            return None
        h = hashlib.md5()
        n = len(r.pages)
        for pg in r.pages:
            c = pg.get_contents()
            h.update(c.get_data() if c is not None else b"")
            h.update(pg.extract_text().encode("utf-8", "replace"))
        return f"{n}:{h.hexdigest()}"
    except Exception:
        return None


def digest(reader, data, ext, tmp):
    if reader == "pillow":
        return pillow_digest(data)
    if reader == "heif":
        return heif_digest(data)
    if reader == "ff":
        return ff_digest(data, ext, tmp)
    if reader == "pdf":
        return pdf_digest(data)
    if reader == "svg":
        return svg_digest(data)
    raise ValueError(reader)


# Per-process state for the pool's workers.
G = {}


def init_worker(sample_path, kind, reader, tmp):
    with open(sample_path, "rb") as f:
        G["d"] = f.read()
    G["reader"] = reader
    G["tmp"] = tmp
    G["ext"] = os.path.splitext(sample_path)[1]
    G["regs"] = structs.FORMATS[kind][0](G["d"])
    G["base"] = digest(reader, G["d"], G["ext"], tmp)
    G["fix"] = structs.FORMATS[kind][1]


def marker(i, length):
    """A marker unique to position i. The 4-byte kind cannot be unique over a large file, but
    is short enough to fit where an 8-byte one would run into something a reader checks."""
    if length == 8:
        return b"Q%06XZ" % i
    alphabet = b"abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
    n = len(alphabet)
    return bytes([alphabet[i % n], alphabet[(i // n) % n], alphabet[(i // n // n) % n], 0x7E])


def try_overwrite(args):
    pos, length = args
    d = G["d"]
    m = marker(pos, length)
    if d[pos:pos + length] == m:
        return None
    mut = d[:pos] + m + d[pos + length:]
    if G["fix"]:
        mut = G["fix"](d, mut, G["regs"], pos, length)
    if digest(G["reader"], mut, G["ext"], G["tmp"]) == G["base"]:
        return (pos, length, mut)
    return None


def try_insert(args):
    label, path, m, mut = args
    if digest(G["reader"], mut, G["ext"], G["tmp"]) == G["base"]:
        return (label, path, m, mut)
    return None


def run_hideme(bin_path, target, outdir, jobs=None):
    os.makedirs(outdir, exist_ok=True)
    cmd = [bin_path, "clean", "--json", "-o", outdir, "--force"]
    if jobs:
        cmd += ["--jobs", str(jobs), "-r"]
    cmd.append(target)
    r = subprocess.run(cmd, capture_output=True)
    return json.loads(r.stdout.decode("utf-8", "replace"))


def classify(res, m, base_warnings):
    st = res.get("status")
    if st != "cleaned":
        return "REFUSED", {"kind": res.get("error_kind")}
    with open(res["output"], "rb") as f:
        out = f.read()
    if m not in out:
        return "REMOVED", {}
    new_warn = [w for w in res.get("warnings", []) if w not in base_warnings]
    if new_warn:
        return "WARNED", {"warn": new_warn[:3]}
    detail = {}
    retained = [f.get("field") for f in res.get("findings", []) if f.get("action") == "retain"]
    if retained:
        detail["retained"] = retained[:3]
    return "SILENT", detail


def sweep_sample(a, name, kind, reader):
    sample_path = os.path.join(a.samples, name)
    with open(sample_path, "rb") as f:
        d = f.read()
    ext = os.path.splitext(name)[1]
    tmp = tempfile.mkdtemp(prefix="sw", dir=a.work)
    work = os.path.join(a.work, name)
    shutil.rmtree(work, ignore_errors=True)
    kept_dir = os.path.join(work, "kept")
    out_dir = os.path.join(work, "out")
    os.makedirs(kept_dir)
    regs = structs.FORMATS[kind][0](d)
    starts = [r[0] for r in regs]
    base = digest(reader, d, ext, tmp)
    if base is None:
        print(f"{name}: the reader cannot read the sample", flush=True)
        return
    baseline = run_hideme(a.bin, sample_path, os.path.join(work, "base"))
    br = baseline["results"][0]
    if br["status"] != "cleaned":
        print(f"{name}: BASELINE REFUSED: {br.get('error_kind')}", flush=True)
        shutil.rmtree(tmp, ignore_errors=True)
        return
    with open(br["output"], "rb") as f:
        base_out = f.read()
    base_warn = set(br.get("warnings", []))
    base_out_digest = digest(reader, base_out, ext, tmp)
    kept = []
    tried = Counter()
    t0 = time.time()
    with mp.Pool(a.jobs, init_worker, (sample_path, kind, reader, tmp)) as pool:
        jobs = []
        for length in a.sizes:
            ps = range(0, max(0, len(d) - length + 1), a.stride)
            tried[f"over{length}"] = len(ps)
            jobs += [(p, length) for p in ps]
        for res in pool.imap_unordered(try_overwrite, jobs, chunksize=8):
            if res is None:
                continue
            pos, length, mut = res
            fn = f"o{length}_{pos}{ext}"
            with open(os.path.join(kept_dir, fn), "wb") as f:
                f.write(mut)
            kept.append({"file": fn, "mode": f"over{length}", "pos": pos, "marker": marker(pos, length).decode("latin1"), "path": structs.locate(regs, starts, pos)})
        if not a.no_insert:
            ins = []
            for i, (label, path, make) in enumerate(structs.FORMATS[kind][2](d)):
                m = b"I%06XZ" % i
                try:
                    mut = make(m)
                except Exception as e:
                    print(f"  insert build failed {label} {path}: {e}", flush=True)
                    continue
                ins.append((label, path, m, mut))
            tried["insert"] = len(ins)
            for res in pool.imap_unordered(try_insert, ins, chunksize=4):
                if res is None:
                    continue
                label, path, m, mut = res
                fn = f"i_{m.decode()}{ext}"
                with open(os.path.join(kept_dir, fn), "wb") as f:
                    f.write(mut)
                kept.append({"file": fn, "mode": "insert", "pos": -1, "marker": m.decode(), "path": f"{path}|{label}"})
    t1 = time.time()
    results = {}
    if kept:
        doc = run_hideme(a.bin, kept_dir, out_dir, jobs=a.jobs)
        for r in doc["results"]:
            results[os.path.basename(r["path"].replace("\\", "/"))] = r
    t2 = time.time()
    rows = []
    for k in kept:
        r = results.get(k["file"])
        if r is None:
            k["class"] = "NORESULT"
        else:
            k["class"], detail = classify(r, k["marker"].encode("latin1"), base_warn)
            k.update(detail)
        rows.append(k)
    rows.sort(key=lambda r: (r["mode"], r["pos"], r["file"]))
    os.makedirs(a.out, exist_ok=True)
    doc = {"name": name, "size": len(d), "tried": dict(tried), "stride": a.stride, "rows": rows,
           "base_warn": sorted(base_warn), "base_same_decode": base_out_digest == base,
           "base_size_out": len(base_out), "secs": [round(t1 - t0, 1), round(t2 - t1, 1)]}
    with open(os.path.join(a.out, name + ".json"), "w") as f:
        json.dump(doc, f, indent=0)
    shutil.rmtree(tmp, ignore_errors=True)
    shutil.rmtree(work, ignore_errors=True)
    print_summary(doc)


def ranges(ps):
    ps = sorted(ps)
    out = []
    for p in ps:
        if out and p == out[-1][1] + 1:
            out[-1][1] = p
        else:
            out.append([p, p])
    return ",".join(f"{a}" if a == b else f"{a}-{b}" for a, b in out[:12]) + (" ..." if len(out) > 12 else "")


def print_summary(doc):
    rows = doc["rows"]
    c = Counter(r["class"] for r in rows)
    print(f"== {doc['name']} size={doc['size']} tried={doc['tried']} kept={len(rows)} {dict(c)} base_decode_ok={doc['base_same_decode']} time={doc['secs']}", flush=True)
    groups = defaultdict(list)
    for r in rows:
        if r["class"] in ("SILENT", "WARNED", "NORESULT"):
            groups[(r["class"], r["path"], r["mode"])].append(r)
    for (cl, p, m), rs in sorted(groups.items()):
        extra = " " + rs[0]["warn"][0][:100] if cl == "WARNED" else ""
        pos = ranges([r["pos"] for r in rs]) if m != "insert" else ""
        print(f"   {cl:8} {m:6} {p:50} n={len(rs):4} {pos}{extra}", flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", default=os.path.join(WORK, "hideme" + EXE), help="the binary under test")
    ap.add_argument("--samples", default=os.path.join(WORK, "samples"), help="what mksamples.py wrote")
    ap.add_argument("--work", default=os.path.join(WORK, "scratch"), help="temporary files, removed as each sample ends")
    ap.add_argument("--out", default=os.path.join(WORK, "results"), help="one JSON file per sample")
    ap.add_argument("--only", default="", help="comma-separated parts of sample names")
    ap.add_argument("--sizes", default="8,4", help="marker lengths: 8, 4 or both")
    ap.add_argument("--stride", type=int, default=1, help="distance between overwritten positions")
    ap.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) - 2))
    ap.add_argument("--no-insert", action="store_true", help="overwrite only")
    a = ap.parse_args()
    a.sizes = [int(x) for x in a.sizes.split(",") if x]
    if not set(a.sizes) <= {4, 8}:
        ap.error("--sizes takes 8, 4 or both")
    os.makedirs(a.work, exist_ok=True)
    names = [n for n in SAMPLES if not a.only or any(k in n for k in a.only.split(","))]
    for n in names:
        kind, reader = SAMPLES[n]
        sweep_sample(a, n, kind, reader)


if __name__ == "__main__":
    mp.freeze_support()
    main()
