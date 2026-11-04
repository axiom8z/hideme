#!/usr/bin/env python
"""Per-format totals of a sweep, as text or as the Markdown table in docs/SWEEP.md, and with
--silent a list of where the SILENT rows fall."""
import argparse
import glob
import json
import os
import re
from collections import Counter, defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(HERE, "work")

FAMILIES = [
    ("JPEG baseline", ["jpeg_baseline", "jpeg_icc_dri"]), ("JPEG progressive", ["jpeg_progressive"]),
    ("PNG", ["png_rgb", "png_palette", "png_icc"]), ("APNG", ["apng"]),
    ("WebP lossy", ["webp_lossy", "webp_simple"]), ("WebP lossless", ["webp_lossless"]), ("WebP animated", ["webp_anim"]),
    ("GIF", ["gif_"]), ("TIFF", ["tiff_"]), ("HEIC", ["heic"]), ("AVIF", ["avif"]),
    ("MP4", ["mp4_"]), ("MOV", ["mov_"]), ("M4A", ["m4a"]), ("MKV", ["mkv"]), ("WebM", ["webm"]), ("AVI", ["avi_"]),
    ("MP3", ["mp3"]), ("FLAC", ["flac"]), ("Vorbis", ["vorbis"]), ("Opus", ["opus"]), ("WAV", ["wav"]), ("AIFF", ["aiff"]),
    ("PDF", ["pdf_"]), ("SVG", ["svg"]), ("DOCX", ["docx"]), ("XLSX", ["xlsx"]), ("PPTX", ["pptx"]), ("ODT", ["odt"]),
]
SHORT = {"REMOVED": "removed", "REFUSED": "refused", "WARNED": "warned", "SILENT": "silent"}


def family(name):
    return next((f for f, keys in FAMILIES if any(name.startswith(k) for k in keys)), None)


def load(dirs):
    for d in dirs:
        for fn in sorted(glob.glob(os.path.join(d, "*.json"))):
            with open(fn) as f:
                yield json.load(f)


def totals(docs):
    out = defaultdict(Counter)
    for doc in docs:
        c = out[family(doc["name"])]
        c["tried"] += sum(doc["tried"].values())
        c["kept"] += len(doc["rows"])
        for r in doc["rows"]:
            c[SHORT.get(r["class"], "other")] += 1
    return out


def print_table(t, markdown):
    cols = ("tried", "kept", "removed", "refused", "warned", "silent")
    heads = ("Sample type", "Tried", "Real hiding spots", "Removed", "Refused", "Warned", "Silent")
    rows = [(f, *(t[f][c] for c in cols)) for f, _ in FAMILIES if f in t]
    rows.append(("Total", *(sum(t[f][c] for f, _ in FAMILIES if f in t) for c in cols)))
    if markdown:
        print("| " + " | ".join(heads) + " |")
        print("|---" + "|--:" * 6 + "|")
        for r in rows:
            label = f"**{r[0]}**" if r[0] == "Total" else r[0]
            print(f"| {label} | " + " | ".join(str(v) for v in r[1:]) + " |")
    else:
        print(f"{'format':18}{'tried':>8}{'kept':>8}{'REM':>8}{'REF':>7}{'WARN':>6}{'SIL':>7}")
        for r in rows:
            print(f"{r[0]:18}{r[1]:8}{r[2]:8}{r[3]:8}{r[4]:7}{r[5]:6}{r[6]:7}")
    other = sum(t[f]["other"] for f in t)
    if other:
        print(f"Rows with no result: {other}")


def group_key(row):
    """Office paths name numbered parts and many positions, so numbers fold together."""
    path = row["path"]
    if row["mode"] == "insert":
        return path.split("|")[0] + " " + path.split("|")[-1]
    return re.sub(r"\d+", "N", path)


def print_silent(docs):
    by_family = defaultdict(Counter)
    for doc in docs:
        for r in doc["rows"]:
            if r["class"] == "SILENT":
                by_family[family(doc["name"])][group_key(r)] += 1
    for f, _ in FAMILIES:
        if f in by_family:
            print(f"== {f}: {sum(by_family[f].values())}")
            for key, n in by_family[f].most_common():
                print(f"{n:7} {key}")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--results", nargs="*", default=[os.path.join(WORK, "results"), os.path.join(WORK, "results_office")])
    ap.add_argument("--markdown", action="store_true")
    ap.add_argument("--silent", action="store_true")
    a = ap.parse_args()
    docs = list(load(a.results))
    if a.silent:
        print_silent(docs)
    else:
        print_table(totals(docs), a.markdown)


if __name__ == "__main__":
    main()
