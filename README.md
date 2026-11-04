# HideMe

HideMe removes privacy-sensitive metadata from files: the GPS position in a photo, the author
of a document, the serial number of a camera, the location track in a phone video. It rewrites
the file's container and never re-encodes the content, so pixels and samples stay as they were.

It is one static binary written in Go, using only the standard library.

```console
$ hideme inspect photo.jpg
photo.jpg: jpeg, 2822 bytes, 6 to remove, 1 retained
  high   Artist: Test Author (at APP1/Exif/IFD0)
  high   GPS coordinates: 48.858400, 2.294500 (at APP1/Exif/GPS)
  medium Camera make: Test Camera Co (at APP1/Exif/IFD0)
  medium Camera model: Test Phone 15 (at APP1/Exif/IFD0)
  medium Comment: secret comment (at COM)
  low    Software: TestEditor 9.1 (at APP1/Exif/IFD0)
  kept   Orientation: 6 (rotated 90 degrees clockwise) (at APP1/Exif/IFD0) [needed to display the image the right way up; strict mode removes it]

$ hideme clean photo.jpg
photo.jpg -> photo.clean.jpg: cleaned, jpeg, 6 removed, 1 retained, 2822 -> 2584 bytes, 3ms
  removed: Artist, GPS coordinates, Camera make, Camera model, Comment, Software
  retained: Orientation
```

## Supported formats

| Kind | Formats |
|---|---|
| Images | JPEG, PNG and APNG, WebP, GIF, TIFF, HEIC and HEIF, AVIF, SVG |
| Video | MP4, MOV, M4V, 3GP, MKV, WebM, AVI |
| Audio | MP3, FLAC, Ogg Vorbis and Opus, WAV, AIFF, M4A |
| Documents | PDF, Word, Excel, PowerPoint, OpenDocument |

Files are recognised by their content, not their name. Anything else, including legacy `.doc`
files, camera raw files and BigTIFF, is refused with an error rather than passed through.

`hideme formats` lists every extension and what each handler cannot remove.

## Usage

Building needs Go 1.27 or later.

```console
$ go build -trimpath -ldflags="-s -w" -o hideme ./cmd/hideme

$ hideme inspect photo.jpg                 # list what is there, changing nothing
$ hideme clean photo.jpg                   # write photo.clean.jpg, leave the original alone
$ hideme clean -r -o cleaned/ ~/Pictures   # clean a folder into a copy of its structure
$ hideme clean --in-place *.jpg            # replace originals by atomic rename
$ hideme clean --strict photo.jpg          # also remove orientation and colour profiles
$ hideme clean --random-name IMG_0412.jpg  # file names are metadata too
$ hideme inspect --json photo.jpg          # one JSON document, for scripts
```

| Exit code | Meaning |
|---|---|
| 0 | Every file was processed and, for `clean`, written and verified |
| 1 | At least one file failed; its original is untouched and no output was left |
| 2 | The command line was wrong; nothing was done |
| 3 | Finished, but some files were unsupported or skipped, so they were not cleaned |

## How it stays trustworthy

The design rule is that **HideMe would rather refuse a file than wrongly call it clean.**

- **Allowlists, not denylists.** Wherever a format allows it, HideMe keeps only what it knows a
  decoder needs, so metadata types invented later are removed too.
- **Verification before the output exists.** Each output is written to a temporary file, then
  checked: its format must be unchanged, reading it back must find no metadata, and a search of
  its raw bytes, independent of any parser, must find no metadata signatures. Only then does it
  get its name. If a check fails, the output is discarded.
- **Originals are safe.** Nothing is overwritten without `--in-place`, and that replaces the file
  atomically. `--in-place` also refuses a file whose cleaned copy might lack content, such as a
  motion photo losing its video, unless you add `--force`.
- **Input is treated as hostile.** Offsets and lengths are bounds-checked, recursion and
  decompression are capped, and metadata printed to the terminal is stripped of escape sequences
  so a file cannot rewrite your screen.
- **Logs stay private.** `clean` prints the names of the fields it removed, not their values, so
  a log does not recreate what was erased.

Some details that matter in practice:

- **Video telemetry is really deleted.** Removing a GPS track means zeroing its samples in the
  media data as well as dropping its index, which would otherwise leave the location log in the
  file. Bytes the sample tables do not reference are cleared too, where the file allows it.
- **PDFs are rebuilt** from the objects still in use, which drops earlier revisions. Those often
  hold text that was deleted or "redacted" in a later save.
- **Multi-gigabyte files need little memory.** Removed regions in MP4, Matroska and AVI are
  replaced in place with padding of the same size, so every offset stays valid and the media is
  streamed through untouched.
- **Embedded files are cleaned too:** photos inside Word documents, PDFs and SVGs keep no EXIF.
- **The download URL Windows records** in a file's `Zone.Identifier` stream is reported by
  `inspect`, and outputs do not inherit it.

## Performance

Cleaning a 2.04 GB MP4, including verification, on a Ryzen 7 3700X with 32 GB of RAM and an NVMe
SSD, with the file already cached:

| Measure | Result, over 4 runs |
|---|---|
| Time | 5.7 to 8.2 seconds (about 240 to 340 MiB/s) |
| Peak working set | 15.5 to 17.1 MB |
| Peak paged memory | 55.5 to 57.6 MB |

A 255 MB MP4 peaked at 53 MB of paged memory, so memory barely grows with file size for formats
that stream. Formats that must be held in memory need more: a million-row spreadsheet took about
1 GB, the worst case measured was about 2 GB, and each parallel job adds its own.

## What it does not do

These limits are stated so that nobody relies on HideMe for something it cannot do.

- **It does not defeat steganography.** Someone deliberately hiding data can still use bytes
  inside encoded streams and unused parts of required fields. [docs/SWEEP.md](docs/SWEEP.md)
  measures this.
- **It does not touch what is visible or audible:** faces, street signs, screenshots of your
  desktop, or a voice.
- **It does not touch data inside encoded streams,** such as an encoder's settings in H.264 SEI
  messages, because removing them would mean re-encoding.
- **Some document content stays, with a warning.** Speaker notes, hidden sheets and text,
  comments, tracked changes, form values, password hashes, macros and links to external files are
  your content, so HideMe keeps them and tells you. **Read the warnings.**
- **`--in-place` cannot erase the old bytes** from SSDs, snapshots, cloud sync or backups. Getting
  rid of an original securely is a separate problem.
- **Removing PDF signatures is unavoidable.** Any rewrite invalidates them, so they are removed,
  with a warning.

## Testing

The tests need only the Go toolchain: over 1,000 test functions and 22 native fuzz targets. The
format handlers' targets assert that if a file cleans, the output has nothing left to report.
[CI](.github/workflows/ci.yml) runs the tests on Linux, macOS and Windows, and under the race
detector on Linux, where tests that measure time or memory are skipped because it distorts both.

Beyond those, the results were checked against software HideMe did not write:

- **Independent readers.** Cleaned files were read back with Pillow, libheif, ffmpeg, pypdf,
  pikepdf, python-docx, openpyxl, python-pptx, odfpy, mutagen and hachoir, comparing decoded pixels
  and audio and video hashes against the originals.
- **ExifTool 13.60 as a writer.** ExifTool wrote a marker into each text tag it would accept, 60,672
  in all, across 32 JPEG, PNG, WebP, GIF, TIFF, HEIC, MP4, MOV, M4A and PDF files, and GPS
  coordinates into 27 of them. After cleaning, none were in ExifTool's reading of the output, and
  no marker was in its raw bytes.
- **ExifTool as a reader.** HideMe cleaned 58 of ExifTool's own test files, 29 of them with maker
  notes from at least 18 camera makers, and ExifTool's reading of each output was checked for what
  survived. HideMe refused the rest of its 194, because they were formats it does not support or
  samples cut down too far to parse.
- **A real iPhone photo and video.** ExifTool found no EXIF, XMP, maker notes or user tags left. The
  photo decoded to identical pixels, every video and audio sample was byte-identical, and all five
  of the video's metadata tracks were zeroed. Neither file held a location, so GPS was planted in
  copies with ExifTool, and that was removed too.
- **LibreOffice 26.8.** 12 cleaned DOCX, XLSX, PPTX, ODT, ODS and ODP files, 6 of them written by
  LibreOffice itself, each opened, exported to PDF with the original's page count and text, and
  saved back to their own format.
- **ONLYOFFICE 10.0.** A cleaned DOCX and XLSX, both written by LibreOffice, opened and displayed
  normally when checked by hand.
- **Adversarial review.** Each format got its own review aimed at breaking it. Bugs had to be
  reproduced, and fixes needed a test that failed first. There has been no independent security
  audit.
- **A byte-level sweep.** It overwrites positions across small samples of 29 file types, keeps
  every change a decoder does not notice, and checks what HideMe does with that data. The harness
  is in [tools/sweep](tools/sweep) and the results and their caveats are in
  [docs/SWEEP.md](docs/SWEEP.md).

**What has not been tested:**

- **Most real devices.** One iPhone photo and one iPhone video are the only files straight from a
  phone. ExifTool's test files are cut down from real cameras' output, and GoPro and Samsung layouts
  were imitated by hand around real encoded media.
- **Apple's own apps.** The cleaned iPhone files were decoded with libheif and ffmpeg, not opened in
  Photos or QuickTime Player.
- **Microsoft Office.** Documents were checked with LibreOffice, ONLYOFFICE and libraries, which may
  be more forgiving, so a repair prompt in Word, Excel or PowerPoint is still possible. No file
  written by Microsoft Office has been opened after cleaning either.
- **Real files on Linux and macOS.** The test suite runs on both, but the checks against real files
  and other programs above were only run on Windows.
- **Symbolic links, FAT drives, cloud placeholder files and a full disk.** Junctions were tested on
  NTFS, but those were not.

## Project layout

```
cmd/hideme/        command-line interface
internal/engine/   walking, workers, atomic writes, verification
internal/core/     the handler contract and shared stream helpers
internal/formats/  one package per format family
internal/exif/     shared parsers (with xmp, riff, audiotag, jpegmeta, textscan)
internal/coretest/ the contract test suite every handler runs
tools/sweep/       the byte-level sweep, ExifTool and LibreOffice checks (Python; not needed to build or test)
docs/DESIGN.md     the rules every handler follows
docs/SWEEP.md      sweep results and how to read them
```

## License

[MIT](LICENSE)
