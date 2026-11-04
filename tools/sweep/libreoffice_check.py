#!/usr/bin/env python
"""Open documents cleaned by HideMe in real LibreOffice and compare them with the originals.

    libreoffice_check.py --bin HIDEME --soffice SOFFICE [--make] [FILE...]

--make also has LibreOffice write its own DOCX, XLSX, ODT and ODS files to test, and re-save
any PPTX given as ODP and PPTX. For each document, LibreOffice exports the original and the cleaned copy to PDF, and their page
counts and extracted text are compared. It then saves the cleaned copy back to its own format,
which only works if it could load the file. LibreOffice runs with a temporary profile, so the
user's own settings are not touched. Needs pypdf (see requirements.txt).

This shows LibreOffice can read the output. It says nothing about Microsoft Office, which is
stricter about what it accepts.
"""
import argparse
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

from pypdf import PdfReader

HTML = """<html><head><title>Planted Title</title><meta name="author" content="Planted Author"></head><body>
<h1>Quarterly notes</h1><p>First paragraph of the document, with <b>bold</b> and <i>italic</i> text.</p>
<table border="1"><tr><td>one</td><td>two</td></tr><tr><td>three</td><td>four</td></tr></table>
<p>Last paragraph.</p></body></html>
"""
CSV = "name,count,price\nalpha,3,1.50\nbeta,7,2.25\ngamma,11,4.00\n"


def convert(soffice, profile, target, src, outdir):
    """Runs one LibreOffice conversion and returns the path it wrote, or None."""
    os.makedirs(outdir, exist_ok=True)
    subprocess.run(
        [soffice, f"-env:UserInstallation={profile}", "--headless", "--norestore", "--nolockcheck",
         "--convert-to", target, "--outdir", outdir, src],
        capture_output=True, timeout=240,
    )
    stem = pathlib.Path(src).stem
    wanted = target.split(":")[0]
    out = os.path.join(outdir, f"{stem}.{wanted}")
    return out if os.path.exists(out) and os.path.getsize(out) > 0 else None


def reading(soffice, profile, path, outdir):
    pdf = convert(soffice, profile, "pdf", path, outdir)
    if not pdf:
        return None
    r = PdfReader(pdf)
    return len(r.pages), [" ".join((p.extract_text() or "").split()) for p in r.pages]


def make_documents(soffice, profile, tmp, given):
    src = os.path.join(tmp, "src")
    os.makedirs(src)
    html, csv = os.path.join(src, "letter.html"), os.path.join(src, "table.csv")
    pathlib.Path(html).write_text(HTML, encoding="utf-8")
    pathlib.Path(csv).write_text(CSV, encoding="utf-8")
    made = []
    out = os.path.join(tmp, "made")
    for target, source, name in (("docx:MS Word 2007 XML", html, "letter.docx"), ("odt", html, "letter.odt"),
                                 ("xlsx", csv, "table.xlsx"), ("ods", csv, "table.ods")):
        p = convert(soffice, profile, target, source, out)
        if p:
            made.append(p)
        else:
            print(f"could not make {name}")
    # LibreOffice cannot start a presentation from plain text, so it re-saves one it is given.
    for path in given:
        if path.lower().endswith(".pptx"):
            for target in ("odp", "pptx"):
                p = convert(soffice, profile, target, path, out)
                if p:
                    made.append(p)
                else:
                    print(f"could not make a {target} from {os.path.basename(path)}")
    return made


def main():
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", required=True, help="the HideMe binary under test")
    ap.add_argument("--soffice", required=True, help="LibreOffice's soffice.com or soffice")
    ap.add_argument("--make", action="store_true", help="also test files LibreOffice writes itself")
    ap.add_argument("files", nargs="*")
    a = ap.parse_args()

    tmp = tempfile.mkdtemp(prefix="hmlo")
    profile = pathlib.Path(os.path.join(tmp, "profile")).as_uri()
    problems = 0
    try:
        files = list(a.files)
        if a.make:
            files += make_documents(a.soffice, profile, tmp, a.files)
        print(f"{'file':26} {'cleaned':8} {'bytes':>15} {'opens':6} {'pages':>7} {'text':6} {'resave':6}")
        for i, path in enumerate(files):
            name = os.path.basename(path)
            ext = pathlib.Path(path).suffix.lstrip(".").lower()
            work = os.path.join(tmp, f"w{i}")
            c = subprocess.run([a.bin, "clean", "--json", "--force", "-o", os.path.join(work, "clean"), path], capture_output=True)
            res = json.loads(c.stdout.decode("utf-8", "replace"))["results"][0]
            if res["status"] != "cleaned":
                problems += 1
                print(f"{name:26} REFUSED  {res.get('reason')}")
                continue
            before = reading(a.soffice, profile, path, os.path.join(work, "pdf-a"))
            after = reading(a.soffice, profile, res["output"], os.path.join(work, "pdf-b"))
            resaved = convert(a.soffice, profile, ext, res["output"], os.path.join(work, "resave"))
            opens = after is not None
            pages = f"{before[0] if before else '?'}->{after[0] if after else '?'}"
            same = bool(before and after and before == after)
            bytes_text = f"{res['bytes_before']}->{res['bytes_after']}"
            print(f"{name:26} {'yes':8} {bytes_text:>15} {'yes' if opens else 'NO':6} {pages:>7} "
                  f"{'same' if same else 'DIFF':6} {'yes' if resaved else 'NO':6}", flush=True)
            problems += (not opens) + (not same) + (not resaved)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    print(f"problems: {problems}")
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
