# Slack-space sweep

The reviews of each handler looked for known places to hide data. This measures instead:
it overwrites bytes all over a file, keeps each change a reader does not notice, and checks
what HideMe does with the data put there. The harness is in `tools/sweep/`.

## Results

| Sample type | Tried | Real hiding spots | Removed | Refused | Warned | Silent |
|---|--:|--:|--:|--:|--:|--:|
| JPEG baseline | 4278 | 2892 | 1552 | 0 | 0 | 1340 |
| JPEG progressive | 2669 | 1620 | 1482 | 0 | 0 | 138 |
| PNG | 3350 | 2123 | 2012 | 102 | 0 | 9 |
| APNG | 1160 | 313 | 267 | 45 | 0 | 1 |
| WebP lossy | 1694 | 1230 | 1228 | 0 | 0 | 2 |
| WebP lossless | 1551 | 1223 | 1222 | 0 | 0 | 1 |
| WebP animated | 2340 | 1264 | 1253 | 1 | 0 | 10 |
| GIF | 946 | 119 | 92 | 15 | 12 | 0 |
| TIFF | 6124 | 2267 | 1773 | 252 | 0 | 242 |
| HEIC | 6828 | 3943 | 2323 | 104 | 3 | 1513 |
| AVIF | 2239 | 1405 | 1359 | 36 | 0 | 10 |
| MP4 | 31344 | 17099 | 7659 | 1709 | 161 | 7570 |
| MOV | 45394 | 5921 | 2451 | 433 | 40 | 2997 |
| M4A | 3307 | 1585 | 1091 | 203 | 13 | 278 |
| MKV | 23974 | 5530 | 2208 | 780 | 4 | 2538 |
| WebM | 9314 | 2552 | 1831 | 660 | 3 | 58 |
| AVI | 56156 | 40804 | 39229 | 245 | 14 | 1316 |
| MP3 | 2802 | 896 | 535 | 0 | 31 | 330 |
| FLAC | 22366 | 16695 | 16647 | 2 | 0 | 46 |
| Vorbis | 8042 | 334 | 260 | 18 | 0 | 56 |
| Opus | 1586 | 269 | 255 | 4 | 0 | 10 |
| WAV | 11174 | 1464 | 1452 | 2 | 0 | 10 |
| AIFF | 10103 | 360 | 337 | 16 | 0 | 7 |
| PDF | 11009 | 6373 | 4696 | 1354 | 19 | 304 |
| SVG | 2894 | 998 | 952 | 0 | 10 | 36 |
| DOCX | 43058 | 27534 | 11710 | 7737 | 56 | 8031 |
| XLSX | 17638 | 8589 | 2894 | 2731 | 163 | 2801 |
| PPTX | 84662 | 33016 | 11549 | 4256 | 1242 | 15969 |
| ODT | 6174 | 4350 | 2447 | 1249 | 14 | 640 |
| **Total** | 424176 | 192768 | 122766 | 21954 | 1785 | 46263 |

The columns:

- **Tried:** changed files built from that type's samples.
- **Real hiding spots:** changed files that an independent reader reads exactly as it reads
  the original. The rest of this table is only about these.
- **Removed:** the output does not hold the marker.
- **Refused:** HideMe failed the file and wrote nothing.
- **Warned:** the marker stays, and the file got a warning the unchanged sample did not.
- **Silent:** the marker stays, and nothing new was said about this file.

## What the numbers mean and do not mean

**Silent does not mean a leak of normal metadata.** Author names, GPS, capture times and the
like live in structures HideMe knows, and those are what its tests and verification cover. A
silent row is a byte a reader ignored that stayed, and these markers were put there on
purpose by this tool.

**Silent does not mean safe either.** Those bytes are still there, mostly inside encoded
streams or fixed-format fields, and someone deliberately hiding data can use them. HideMe
removes metadata; it does not defeat steganography. The limitations each handler states
describe these places, and `hideme formats` prints them.

**The checker is blind to some real content, which inflates the hiding spots and the silent
count.** A row counts as a hiding spot when the reader's result does not change, and each
reader looks at only part of a file:

- **Images:** Pillow's decoded pixels, frames and palette colours. Not pixel density or gamma
  (`pHYs`, `gAMA`).
- **Audio and video:** an MD5 of what ffmpeg decodes, which is sample values and nothing about
  time. Movie and track durations, timing tables and display sizes (`mvhd`, `tkhd`, `stts`)
  count as hiding spots, and so do the sample rates in WAV, FLAC, AIFF and Vorbis headers,
  though changing one changes how fast the audio plays.
- **PDF:** pypdf's page content and extracted text, not rendering. Font names and encodings
  (`/BaseFont`, `/Encoding`) count, and so does `/PageMode`.
- **Office:** the text and cell values the Python libraries return. Formatting attributes,
  styles, themes, font tables, relationship types and content types all count, though Office
  uses them, and it may repair or refuse a file with a changed one. Most of the Office silent
  rows, which are most of the silent rows in the table, are of this kind.
- **SVG:** the tree of SVG elements, attributes and drawn text. `title` and `desc` are left out,
  and those rows are also reported to the user as retained.

So some "hiding spots" are real content that a fuller reader would use, and changing them
would visibly change the file.

**These are counts of attempts, not of bytes or of risk.** Windows overlap, so one byte can
appear in up to 12 rows. The samples are tiny and few, so the totals say nothing about how
much room a real file has, and formats cannot be ranked by them. A format's largest group is
usually its encoded data, and that grows with the file.

**It is not exhaustive.** Each row puts one marker in one place, and the markers do not cover
compressed data, since overwriting that mostly breaks it: data re-encoded into a compressed
stream is not measured here. A different reader may react to bytes these do not, and insertion
tries only the kinds of structure listed below.

**Refused is the safe outcome, but it costs the user the file.** These are changed files only.
Every unchanged sample is cleaned; see below.

## Where the silent rows are

The groups a type's rows mostly fall in, largest first. The limitations each handler states,
printed by `hideme formats`, cover these.

| Sample type | Silent rows fall in |
|---|---|
| JPEG | Huffman and quantisation table values; 7 bytes of the JFIF header |
| PNG, APNG | Pixel density, gamma, animation loop count |
| WebP | Bytes after the stream inside its chunk; animation and frame header values |
| TIFF | `JPEGTables` values, nearly all of them; `RowsPerStrip`; strip data |
| HEIC, AVIF | A retained ICC profile (1166 of HEIC's rows, all from one sample); `hvcC`; encoded data; pixel depth and colour values |
| MP4, MOV, M4A | Encoded data in `mdat`, then `tkhd`, `mvhd`, decoder configuration, sample tables and the fragment index |
| MKV, WebM | Block payloads, then codec private data, track UIDs, durations and sample rates |
| AVI | Encoded frames, then stream and main headers, video properties and stream formats |
| MP3 | The Xing or Info header, and data inside frames |
| FLAC | `STREAMINFO`: the MD5, size hints, sample count and rate |
| Vorbis, Opus | The Vorbis setup header and identification header; page fields such as granule positions |
| WAV, AIFF | Rate and frame count fields in `fmt ` and `COMM` |
| PDF | Values of defined keys in page, font and catalog dictionaries; content streams |
| SVG | Text in `title` and `desc` |
| DOCX, XLSX, PPTX, ODT | Relationship and content types, namespace addresses, attribute names and values, element text; then markup and text inserted under the format's own namespaces |

## Unchanged samples

`tools/sweep/baseline.py` cleans each of the 50 unchanged samples in default and in strict mode,
checks that a reader still reads the output the same, and cleans the output again.

- **Refusals:** none, in either mode.
- **Second clean:** identical bytes for all 50, in both modes.
- **Reading:** the same for 49. The cleaned `mp3.mp3` decodes to an exact prefix of what the
  original decodes to under ffmpeg: 17640 bytes of samples against 18526, and 17640 is the 0.2
  seconds encoded. Its bytes are exactly the original's frames, with only the tags gone. ffmpeg
  calls the original an "invalid concatenated file" and estimates 0.27 seconds, and reads the
  output as 0.20, so it decodes padding from the original that it trims from the output. The
  script counts this as a difference, so it exits 1.

## Method

Built from this tree with go1.27.3 for windows/amd64 and run on Windows 10, with Python 3.14.6,
the packages pinned in `tools/sweep/requirements.txt`, and ffmpeg 9.0.2 from `imageio-ffmpeg` 0.8.1.

**Samples.** `mksamples.py` builds 50 small files of 100 bytes to 35 KB with fake metadata: 46
image, audio, video, PDF and SVG samples and 4 Office ones. The sample set's sha256 was
`fae2078fd20d53af00b7c013f81c21bb6de9c1cf9818525b76957c66ae04ebfd`.

**Image, audio, video, PDF and SVG (`sweep.py`, stride 1, sizes 8 and 4).** For each sample:

1. Overwrite 8 bytes at every position with a marker unique to it, `Q` then six hex digits then
   `Z`. Then do the same with a 4-byte marker, three letters then `~`, which fits where 8 bytes
   would reach something a reader checks. PNG, FLAC and Ogg checksums are made valid again,
   unless the marker is on the checksum.
2. Insert a marker, `I` then six hex digits then `Z`, wherever `structs.py` finds a place:
   unknown and free boxes, chunks and segments at every structural position, comments, private
   tags and keys, bytes at the end of each structure with its lengths and offsets repaired, and
   data after the end of the file.
3. Keep the file if its reader gives the same result as for the original: Pillow for images,
   pillow-heif for HEIC, ffmpeg for audio and video, pypdf for PDF, an XML parse for SVG.

**Office (`office_sweep.py`, `--max-per-part 1500`, sizes 8 and 4).** The same, on the
decompressed `.xml` and `.rels` parts, with the ZIP rebuilt each time:

- Each part is overwritten at a stride of its size divided by 1500, rounded down and at least 1,
  so parts under 3000 bytes get every position. The largest stride was 292, in DOCX.
- Inserts go at up to about 24 tags per part: comments, processing instructions, text, CDATA,
  elements and attributes in foreign and in the format's own namespaces. Also relationships, new
  parts, and at ZIP level a comment, an entry comment, an extra field, and data before and after.
- The readers are python-docx, openpyxl, python-pptx and odfpy.

**Classifying.** The real binary runs `clean --json --force -o DIR` on every kept file:

- `Refused`: the status is not `cleaned`.
- `Removed`: the marker is not in the output's raw bytes. For Office it must also be absent from
  the decompressed parts and the part names.
- `Warned`: the marker stays, and there is a warning the unchanged sample did not get.
- `Silent`: the marker stays, and there is no such warning.

To repeat it, see `tools/sweep/README.md`.
