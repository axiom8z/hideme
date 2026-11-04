# Slack-space sweep

Measures where readers ignore bytes in each supported format, and what HideMe does with
data put there. `docs/SWEEP.md` holds the results and says what they mean. This is a test
tool only: the Go build and the Go tests do not use it.

## Running it

From the repository root, with Python 3.14:

```
python -m venv tools/sweep/venv
tools/sweep/venv/Scripts/python -m pip install -r tools/sweep/requirements.txt   # bin/python elsewhere
go build -o tools/sweep/work/hideme.exe ./cmd/hideme                             # hideme elsewhere

cd tools/sweep
venv/Scripts/python mksamples.py
venv/Scripts/python sweep.py
venv/Scripts/python office_sweep.py
venv/Scripts/python baseline.py
venv/Scripts/python table.py --markdown
venv/Scripts/python table.py --silent
```

The two sweeps take about 20 minutes on 16 logical processors, and the settings above are
the defaults: every byte position with 8-byte and 4-byte markers, and in Office
`--max-per-part 1500`, a stride of the part's size divided by 1500, rounded down.
`--help` lists the rest. ffmpeg comes from the `imageio-ffmpeg` package,
whose build differs by platform. `mksamples.py` fixes random IDs and times, and prints a
digest of the sample set, which `docs/SWEEP.md` gives for its run.

Everything is written under `tools/sweep/work/`, which git ignores. Only the final table is
meant to be kept, in `docs/SWEEP.md`.

## Files

| File | What it does |
|---|---|
| `mksamples.py` | Builds small valid samples of every format with fake metadata |
| `structs.py` | Independent parsers: where regions are, checksum repairs, what to insert where |
| `sweep.py` | Sweeps image, audio, video, PDF and SVG samples |
| `office_sweep.py` | Sweeps the decompressed parts of DOCX, XLSX, PPTX and ODT samples |
| `baseline.py` | False-refusal, reading and idempotence check on unmodified samples |
| `table.py` | Totals per format, and where the silent rows fall |
| `exiftool_plant.py` | Has ExifTool write every text tag it can, plus GPS, then checks cleaning removes them |
| `exiftool_check.py` | Cleans files and asks ExifTool what survived, with `--private` for personal files |
| `libreoffice_check.py` | Opens cleaned documents in LibreOffice and compares them with the originals |

## Checks with other tools

These need ExifTool or LibreOffice installed and take paths to them. The two ExifTool scripts
use only the standard library, and `libreoffice_check.py` needs `pypdf`.

```
python exiftool_plant.py --bin HIDEME --exiftool EXIFTOOL FILE...
python exiftool_check.py --bin HIDEME --exiftool EXIFTOOL FOLDER_OR_FILE...
python libreoffice_check.py --bin HIDEME --soffice SOFFICE --make DOCUMENT...
```

ExifTool's own test files are in the `t/images` folder of its source package. It cannot write
to fragmented MP4, so `exiftool_plant.py` reports those samples as writes that failed.

`exiftool_check.py` lists every tag that survives, most of them image or track structure that
a reader needs, so its output is for reading rather than a pass or fail. A "hidden" result
means bytes from a removed tag are still in the file, and each one needs explaining: two
tracks with the same timing, for example, hold the same table, so removing one leaves its
bytes in the other.
