#!/usr/bin/env python
"""Have ExifTool write metadata into copies of files, then check HideMe removes all of it.

    exiftool_plant.py --bin HIDEME --exiftool EXIFTOOL [--private] FILE...

ExifTool writes a numbered marker into every writable text tag it knows for the file's format,
in batches, using the layouts it normally writes, and also writes GPS coordinates and dates.
Each copy is cleaned, then two checks look for what was planted: ExifTool's reading of the
output, and a search of the output's raw bytes for the marker.

Inputs are never changed. --private numbers files instead of naming them; nothing here prints
a value read from a file in either mode, only tag names and counts. Standard library only.
"""
import argparse
import concurrent.futures
import json
import mmap
import os
import shutil
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET

MARKER = "HMPLANT"
BATCH = 300

# What ExifTool can write to each format, by family 0 group.
GROUPS = {
    ".jpg": ["EXIF", "XMP", "IPTC", "Photoshop"],
    ".jpeg": ["EXIF", "XMP", "IPTC", "Photoshop"],
    ".tif": ["EXIF", "XMP", "IPTC"],
    ".tiff": ["EXIF", "XMP", "IPTC"],
    ".png": ["PNG", "XMP", "EXIF"],
    ".webp": ["EXIF", "XMP"],
    ".gif": ["XMP"],
    ".heic": ["EXIF", "XMP", "QuickTime"],
    ".mp4": ["QuickTime", "XMP"],
    ".mov": ["QuickTime", "XMP"],
    ".m4a": ["QuickTime", "XMP"],
    ".pdf": ["PDF", "XMP"],
}
GPS_STILL = ["-GPSLatitude=48.8584", "-GPSLatitudeRef=N", "-GPSLongitude=2.2945", "-GPSLongitudeRef=E",
             "-GPSAltitude=35", "-XMP:GPSLatitude=48.8584 N", "-XMP:GPSLongitude=2.2945 E",
             "-EXIF:DateTimeOriginal=2001:02:03 04:05:06", "-EXIF:CreateDate=2001:02:03 04:05:06"]
GPS_MOVIE = ["-Keys:GPSCoordinates=48.8584 N, 2.2945 E, 35", "-ItemList:GPSCoordinates=48.8584 N, 2.2945 E, 35",
             "-UserData:GPSCoordinates=48.8584 N, 2.2945 E, 35", "-XMP:GPSLatitude=48.8584 N",
             "-XMP:GPSLongitude=2.2945 E", "-Keys:CreationDate=2001:02:03 04:05:06"]
MOVIES = (".mp4", ".mov", ".m4a")


def writable_tags(exiftool):
    """Text tags ExifTool can write, as group-qualified names, by family 0 group."""
    r = subprocess.run([exiftool, "-listx"], capture_output=True, check=True)
    tags = {}
    for table in ET.fromstring(r.stdout).iter("table"):
        g0, g1 = table.get("g0"), table.get("g1")
        for tag in table.iter("tag"):
            if tag.get("writable") != "true" or tag.get("type") not in ("string", "lang-alt"):
                continue
            # XMP and QuickTime need the namespace or atom; for the others ExifTool chooses.
            group = (tag.get("g1") or g1) if g0 in ("XMP", "QuickTime") else g0
            tags.setdefault(g0, set()).add(f"{group}:{tag.get('name')}")
    return {g: sorted(names) for g, names in tags.items()}


def run_exiftool(binary, args, workdir):
    fd, name = tempfile.mkstemp(suffix=".args", dir=workdir)
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(args) + "\n")
    r = subprocess.run([binary, "-charset", "filename=utf8", "-@", name], capture_output=True)
    os.remove(name)
    return r


def read(binary, path, workdir):
    r = run_exiftool(binary, ["-j", "-a", "-G1", "-s", "-e", "-b", "-ee3", path], workdir)
    text = r.stdout.decode("utf-8", "replace").strip()
    return json.loads(text, object_pairs_hook=list, strict=False)[0] if text else []


def marked(doc):
    return sorted({k for k, v in doc if MARKER in str(v)})


def raw_hits(path):
    if os.path.getsize(path) == 0:
        return 0
    with open(path, "rb") as f, mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as m:
        wide = MARKER.encode("utf-16-le")
        big = MARKER.encode("utf-16-be")
        return sum(m.find(needle) >= 0 for needle in (MARKER.encode(), wide, big))


def location_tags(doc):
    return sorted({k for k, _ in doc if "GPS" in k.upper()})


def one_batch(a, path, args, kind, workdir):
    """Plant, clean and check one batch. Returns a dict of counts and tag names."""
    tmp = tempfile.mkdtemp(prefix="plant", dir=workdir)
    res = {"kind": kind, "planted": 0, "survived": [], "raw": 0, "refused": None, "write_failed": False,
           "location_before": [], "location_after": []}
    try:
        ext = os.path.splitext(path)[1].lower()
        planted = os.path.join(tmp, "planted" + ext)
        r = run_exiftool(a.exiftool, ["-m", "-o", planted] + args + [path], tmp)
        if not os.path.exists(planted):
            res["write_failed"] = True
            return res
        before = read(a.exiftool, planted, tmp)
        res["planted"] = len(marked(before))
        res["location_before"] = location_tags(before)
        c = subprocess.run([a.bin, "clean", "--json", "-o", os.path.join(tmp, "out"), "--force", planted], capture_output=True)
        out = json.loads(c.stdout.decode("utf-8", "replace"))["results"][0]
        if out["status"] != "cleaned":
            res["refused"] = out.get("reason")
            return res
        after = read(a.exiftool, out["output"], tmp)
        res["survived"] = marked(after)
        res["raw"] = raw_hits(out["output"])
        res["location_after"] = location_tags(after)
        return res
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def main():
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", required=True, help="the HideMe binary under test")
    ap.add_argument("--exiftool", required=True, help="the ExifTool binary")
    ap.add_argument("--private", action="store_true", help="number files instead of naming them")
    ap.add_argument("--workers", type=int, default=os.cpu_count() or 4)
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()

    catalogue = writable_tags(a.exiftool)
    print("writable text tags ExifTool lists: " + ", ".join(f"{g} {len(catalogue[g])}" for g in sorted(catalogue) if g in
          {g for groups in GROUPS.values() for g in groups}), flush=True)
    workdir = tempfile.mkdtemp(prefix="hmplant")
    problems = total_planted = 0
    try:
        for index, path in enumerate(a.files, 1):
            ext = os.path.splitext(path)[1].lower()
            label = f"[{index}] {ext}" if a.private else os.path.basename(path)
            if ext not in GROUPS:
                print(f"{label}: ExifTool cannot write this format; skipped", flush=True)
                continue
            names = [n for g in GROUPS[ext] for n in catalogue.get(g, [])]
            jobs = []
            for start in range(0, len(names), BATCH):
                args = [f"-{n}={MARKER}{start + i}" for i, n in enumerate(names[start:start + BATCH])]
                jobs.append((args, "text"))
            if ext != ".pdf" and ext != ".gif":
                jobs.append((GPS_MOVIE if ext in MOVIES else GPS_STILL, "location"))
            with concurrent.futures.ThreadPoolExecutor(a.workers) as pool:
                results = list(pool.map(lambda j: one_batch(a, path, j[0], j[1], workdir), jobs))

            planted = sum(r["planted"] for r in results)
            total_planted += planted
            survived = sorted({t for r in results for t in r["survived"]})
            raw = sum(1 for r in results if r["raw"])
            refused = [r for r in results if r["refused"]]
            failed = sum(r["write_failed"] for r in results)
            # Only the batch that writes real coordinates counts, so a text tag that merely
            # has GPS in its name cannot pass for one.
            location = [r for r in results if r["kind"] == "location" and not r["write_failed"]]
            loc_before = sorted({t for r in location for t in r["location_before"]})
            loc_after = sorted({t for r in location for t in r["location_after"]})
            print(f"{label}: {len(results)} batches, {planted} marked tags written, {len(survived)} survived, "
                  f"raw marker in {raw} outputs, {len(refused)} batches refused, {failed} writes failed; "
                  f"GPS tags {len(loc_before)} -> {len(loc_after)}", flush=True)
            for t in survived:
                print(f"    SURVIVED {t}")
            for t in loc_after:
                print(f"    LOCATION {t}")
            for reason in sorted({r["refused"] for r in refused}):
                print(f"    refused: {reason}")
            problems += len(survived) + raw + len(loc_after)
            if location and not loc_before:
                print("    NO GPS WAS WRITTEN, so this file's location check proves nothing")
                problems += 1
    finally:
        shutil.rmtree(workdir, ignore_errors=True)
    print(f"\nin all: {total_planted} marked tags written; problems: {problems}")
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
