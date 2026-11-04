#!/usr/bin/env python
"""Check HideMe's output with ExifTool, which shares no code with it.

    exiftool_check.py --bin HIDEME --exiftool EXIFTOOL [--private] [--strict] PATH...

Each file is cleaned into a temporary folder, the original is left alone, and ExifTool reads
both copies. For each file it reports:

  survivors  tags ExifTool still finds in the output, leaving out file-system facts and
             what it derives from the file's structure
  residue    values ExifTool read from the original that are still in the output's raw bytes.
             "hidden" means ExifTool no longer reports that tag, so only the bytes remain
  hash       whether ExifTool's hash of the image or media data changed
  warnings   validation warnings the output has and the original did not

--private is for real personal files: it numbers files instead of naming them and prints tag
names only, never values. Standard library only.
"""
import argparse
import base64
import collections
import json
import mmap
import os
import shutil
import subprocess
import sys
import tempfile

# Facts about the file itself, not metadata stored in it.
IGNORED_GROUPS = {"System", "ExifTool", ""}
DERIVED = {
    "File:FileType", "File:FileTypeExtension", "File:MIMEType", "File:ImageWidth",
    "File:ImageHeight", "File:EncodingProcess", "File:BitsPerSample", "File:ColorComponents",
    "File:YCbCrSubSampling",
}
HASH_TAG = "ImageDataHash"


def exiftool(binary, args, tmp):
    """Runs ExifTool with its arguments in a UTF-8 file, so any file name survives on Windows."""
    fd, name = tempfile.mkstemp(suffix=".args", dir=tmp)
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(args) + "\n")
    r = subprocess.run([binary, "-charset", "filename=utf8", "-@", name], capture_output=True)
    os.remove(name)
    text = r.stdout.decode("utf-8", "replace").strip()
    if not text:
        return []
    # Pairs rather than dicts: with -a, duplicate tag names are the point.
    return json.loads(text, object_pairs_hook=list, strict=False)[0]


def tags(doc):
    for key, value in doc:
        group, _, name = key.rpartition(":")
        yield group, name, value


def read_all(binary, path, tmp):
    return exiftool(binary, [
        "-j", "-a", "-G1", "-s", "-e", "-b", "-U", "-ee3",
        "-api", "LargeFileSupport=1",
        "-api", "RequestTags=" + HASH_TAG,
        "-api", "ImageHashType=SHA256",
        path,
    ], tmp)


def read_warnings(binary, path, tmp):
    doc = exiftool(binary, ["-j", "-a", "-G1", "-s", "-validate", "-warning", "-error", path], tmp)
    return {str(v) for _, n, v in tags(doc) if n in ("Warning", "Error")}


def is_metadata(group, name):
    return group not in IGNORED_GROUPS and f"{group}:{name}" not in DERIVED and name != HASH_TAG


def needles(value):
    """The byte strings a value would appear as in a file."""
    if isinstance(value, (list, tuple)):
        for v in (value[1:] if isinstance(value, tuple) else value):
            yield from needles(v)
    elif isinstance(value, str):
        if value.startswith("base64:"):
            data = base64.b64decode(value[7:])
            if len(data) >= 16 and len(set(data)) >= 4:
                yield data
        elif len(value) >= 6 and len(set(value)) >= 3:
            yield value.encode("utf-8", "replace")
            yield value.encode("utf-16-le", "replace")
            yield value.encode("utf-16-be", "replace")


def contains_any(path, wanted):
    """Which of the wanted needles occur in the file, without loading a large file into memory."""
    if not wanted or os.path.getsize(path) == 0:
        return set()
    with open(path, "rb") as f, mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as m:
        return {n for n in wanted if m.find(n) >= 0}


def show(value):
    if isinstance(value, list):
        return "; ".join(show(v) for v in value)
    text = str(value)
    if text.startswith("base64:"):
        return f"<binary, {len(text) * 3 // 4} bytes>"
    return text.replace("\n", " ")[:70]


def hash_of(doc):
    for _, name, value in tags(doc):
        if name == HASH_TAG:
            return value
    return None


def check(a, index, path, totals):
    label = f"[{index}] {os.path.splitext(path)[1].lower()}" if a.private else os.path.basename(path)
    tmp = tempfile.mkdtemp(prefix="etc")
    try:
        cmd = [a.bin, "clean", "--json", "-o", os.path.join(tmp, "out"), "--force"]
        cmd += ["--strict"] if a.strict else []
        cmd += [] if a.private else ["-v"]
        r = subprocess.run(cmd + [path], capture_output=True)
        res = json.loads(r.stdout.decode("utf-8", "replace"))["results"][0]
        if res["status"] != "cleaned":
            totals["refused"] += 1
            print(f"{label}: REFUSED ({res['status']}): {res.get('reason')}", flush=True)
            return
        totals["cleaned"] += 1

        before, after = read_all(a.exiftool, path, tmp), read_all(a.exiftool, res["output"], tmp)
        survivors = collections.defaultdict(list)
        for group, name, value in tags(after):
            if is_metadata(group, name):
                survivors[f"{group}:{name}"].append(value)

        wanted = collections.defaultdict(set)
        for group, name, value in tags(before):
            if is_metadata(group, name):
                for needle in needles(value):
                    wanted[needle].add(f"{group}:{name}")
        hits = contains_any(res["output"], set(wanted))
        found = {tag for needle in hits for tag in wanted[needle]}
        # Bytes a kept tag also holds are explained by that tag, as when a removed track
        # has the same edit list as a kept one.
        hidden = sorted({t for needle in hits if not wanted[needle] & set(survivors) for t in wanted[needle]})

        hb, ha = hash_of(before), hash_of(after)
        state = "n/a" if hb is None or ha is None else ("same" if hb == ha else "CHANGED")
        new_warnings = read_warnings(a.exiftool, res["output"], tmp) - read_warnings(a.exiftool, path, tmp)

        print(f"{label}: cleaned, {res['bytes_before']} -> {res['bytes_after']} bytes, "
              f"tags {sum(is_metadata(g, n) for g, n, _ in tags(before))} -> "
              f"{sum(len(v) for v in survivors.values())}, hash {state}", flush=True)
        for tag in sorted(survivors):
            totals["survivors"][tag] += 1
            print(f"    survivor {tag}" + ("" if a.private else f" = {show(survivors[tag])}"))
        for tag in hidden:
            totals["hidden"][tag] += 1
            print(f"    RESIDUE hidden {tag}")
        for tag in sorted(found - set(hidden)):
            print(f"    residue, still a tag {tag}")
        if a.private:
            if new_warnings:
                print(f"    {len(new_warnings)} new warning(s)")
        else:
            for w in sorted(new_warnings):
                print(f"    NEW WARNING {w}")
        totals["new_warnings"] += len(new_warnings)
        totals["hash_" + state] += 1
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def main():
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", required=True, help="the HideMe binary under test")
    ap.add_argument("--exiftool", required=True, help="the ExifTool binary")
    ap.add_argument("--private", action="store_true", help="tag names only, never values or file names")
    ap.add_argument("--strict", action="store_true", help="clean with --strict")
    ap.add_argument("paths", nargs="+", help="files, or folders whose top-level files are checked")
    a = ap.parse_args()

    files = []
    for p in a.paths:
        if os.path.isdir(p):
            files += sorted(os.path.join(p, n) for n in os.listdir(p) if os.path.isfile(os.path.join(p, n)))
        else:
            files.append(p)

    totals = collections.defaultdict(int)
    totals["survivors"] = collections.Counter()
    totals["hidden"] = collections.Counter()
    for i, path in enumerate(files, 1):
        check(a, i, path, totals)

    print(f"\n{len(files)} files: {totals['cleaned']} cleaned, {totals['refused']} refused; "
          f"hash same {totals['hash_same']}, changed {totals['hash_CHANGED']}, n/a {totals['hash_n/a']}; "
          f"new warnings {totals['new_warnings']}; hidden residue tags {sum(totals['hidden'].values())}")
    if totals["survivors"]:
        print("survivors, by how many files have them:")
        for tag, n in sorted(totals["survivors"].items(), key=lambda kv: (-kv[1], kv[0])):
            print(f"  {n:4} {tag}")


if __name__ == "__main__":
    main()
