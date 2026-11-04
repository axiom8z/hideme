#!/usr/bin/env python
"""False-refusal and content check on the unmodified samples.

Cleans every sample in default and strict modes, compares each output's reading with the
sample's, and cleans the output again to check idempotence. Exits 1 if anything is wrong.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

import office_sweep
import sweep

OFFICE = {".docx": "docx", ".xlsx": "xlsx", ".pptx": "pptx", ".odt": "odt"}


def digest(reader, data, ext, tmp):
    if ext in OFFICE:
        return office_sweep.digest(OFFICE[ext], data)
    return sweep.digest(reader, data, ext, tmp)


def run(binary, path, out, strict):
    cmd = [binary, "clean", "--json", "-v", "-o", out, "--force"] + (["--strict"] if strict else []) + [path]
    r = subprocess.run(cmd, capture_output=True)
    return r.returncode, json.loads(r.stdout.decode("utf-8", "replace"))["results"][0]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", default=os.path.join(sweep.WORK, "hideme" + sweep.EXE), help="the binary under test")
    ap.add_argument("--samples", default=os.path.join(sweep.WORK, "samples"), help="what mksamples.py wrote")
    a = ap.parse_args()
    os.makedirs(sweep.WORK, exist_ok=True)
    tmp = tempfile.mkdtemp(prefix="bl", dir=sweep.WORK)
    problems = refused = 0
    for name in sorted(os.listdir(a.samples)):
        path = os.path.join(a.samples, name)
        _, reader = sweep.SAMPLES.get(name, (None, None))
        ext = os.path.splitext(name)[1]
        if ext in OFFICE:
            reader = "office"
        with open(path, "rb") as f:
            data = f.read()
        base = digest(reader, data, ext, tmp) if reader else None
        line = [f"{name:22}"]
        for strict in (False, True):
            tag = "strict" if strict else "default"
            code, res = run(a.bin, path, os.path.join(tmp, tag), strict)
            if res["status"] != "cleaned":
                problems += 1
                refused += 1
                line.append(f"{tag}: REFUSED exit={code} {res.get('reason')}")
                continue
            with open(res["output"], "rb") as f:
                out = f.read()
            state = "ok"
            if reader and base is not None and digest(reader, out, ext, tmp) != base:
                state = "READS-DIFFERENTLY"
                problems += 1
            _, again = run(a.bin, res["output"], os.path.join(tmp, "again"), strict)
            if again["status"] != "cleaned":
                state += f" SECOND-CLEAN-REFUSED {again.get('reason')}"
                problems += 1
            else:
                with open(again["output"], "rb") as f:
                    if f.read() != out:
                        state += " NOT-IDEMPOTENT"
                        problems += 1
            line.append(f"{tag}: {state} {len(data)}->{len(out)} warnings={len(res.get('warnings', []))}")
        print(" ".join(line), flush=True)
    shutil.rmtree(tmp, ignore_errors=True)
    print(f"refused: {refused}, problems in all: {problems}")
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
