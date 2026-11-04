# HideMe design

HideMe removes privacy-sensitive metadata from files. It is a single static Go
binary built only on the standard library.

This document is the source of truth for how the code is organised and which
rules every format handler follows.

## Goals, in priority order

1. **Never claim a file is clean when it is not.** If HideMe cannot be sure, it
   fails with an error and writes nothing. Being wrong here can hurt someone.
2. **Never damage the content.** Pixels, audio and video samples, and document
   text are preserved bit for bit. HideMe rewrites containers. It never
   re-encodes media.
3. **Never touch the original** unless the user passes `--in-place`, and then
   only through an atomic replace, so a crash cannot lose the file.
4. **Treat every input as hostile.** Files are attacker-controlled: no panics,
   no unbounded allocation, no unbounded recursion, and no terminal escape
   injection through metadata values.

## Layout

```
cmd/hideme/            CLI: argument parsing and output only
internal/core/         Handler contract, report types, registry, stream helpers
internal/engine/       Walks paths, runs workers, writes atomically, verifies
internal/coretest/     Contract test suite every handler runs
internal/exif/         EXIF reader, GPS decoding, minimal-EXIF writer
internal/xmp/          XMP packet reader
internal/riff/         RIFF chunk iterator (WebP, WAV, AVI)
internal/audiotag/     ID3, APE, Vorbis comment and other tag decoding for audio
internal/jpegmeta/     JPEG segment rules, Photoshop resources and IPTC (JPEG, TIFF)
internal/textscan/     Looks for readable text in data a handler must keep (AVI, Matroska)
internal/formats/      One package per format family
internal/formats/all/  Blank-imports every handler package
tools/sweep/           Python harness behind docs/SWEEP.md; nothing in Go depends on it
```

Dependencies point one way: `cmd` -> `engine` -> `core`, and `formats/*` -> `core`, `exif`,
`xmp`, `riff`, `audiotag`, `jpegmeta`, `textscan`. Handler packages never import one
another. To clean an embedded file, they call `core.CleanBytes`, which finds the
right handler through the registry.

## The handler contract

See `internal/core/core.go` for the interface. A handler must obey these rules.

### Fail closed

- Use an **allowlist** wherever the format allows it. Keep only what is known to
  be needed and drop everything else, so metadata types invented next year are
  removed too. A denylist is acceptable only where the format makes an allowlist
  impossible, and the code comment must say why.
- If the structure is not fully understood, return `core.ErrMalformed`. Do not
  guess and do not pass bytes through.
- Bytes after the logical end of a file (past the JPEG EOI, PNG IEND, RIFF size
  and so on) are dropped. Phones and steganography tools hide data there.
- A kept structure is held to what its own fields account for. Bytes past that,
  reserved fields, and markup a format does not define are zeroed or removed, because
  a reader skips them. Where that cannot be told, as with the value of a field a
  reader may use, `Format.Limitations` says so and says how much it can hold.
- Encrypted content returns `core.ErrEncrypted`. Variants the handler cannot
  parse return `core.ErrUnsupported`. **Never** pass such a file through.
- A metadata block that is going anyway need not be understood. Damage inside one makes
  the report less specific, not the file refused, because nothing of it is kept.

### Inspect and Clean must agree

`Inspect` and `Clean` share one parser. `Inspect` on a handler's own output must
return no finding whose action is not `retain`. The engine enforces this after
every clean and discards the output if it fails, so a handler that misses
something fails loudly instead of quietly leaking.

Every finding needs a category, a risk and an action. Prefer specific, decoded
values, because they are the product's value to the user: report
`GPS coordinates: 48.85837, 2.29448`, not `EXIF block, 4 KiB`.

### Lossless and deterministic

- `Clean` must not change decoded content. Each handler tests this as strongly
  as the standard library allows, such as pixel equality for JPEG, PNG and GIF.
- Same input and options produce identical output bytes. Use no timestamps, no
  randomness, and no map iteration order in anything written out.
- Cleaning a cleaned file produces identical bytes (idempotence).

### Retained data

By default HideMe keeps two kinds of data, because removing them visibly damages
the result: **image orientation** (otherwise phone photos appear sideways) and
**ICC colour profiles** (otherwise wide-gamut photos look wrong). Both are
reported with `ActionRetain` and a `Reason`. `Options.Strict` removes them too,
including HEIF's `irot` and `imir` properties, which are where that format keeps
orientation. A video's rotation is a matrix in fixed-size fields and stays even so, which
the handler's limitations say.

Orientation is kept by writing a new, minimal EXIF block holding that one tag,
never by filtering the original block. Everything else in the original is gone. Damage
elsewhere in a block does not stop its orientation being read, since viewers read it all
the same and a number from 1 to 8 can carry nothing out of the block.

### Offsets in large containers

MP4, HEIC, Matroska and AVI contain absolute offsets into themselves. Instead of
recalculating them, HideMe keeps every offset valid by **replacing removed bytes
in place**: a `free` box in ISOBMFF, a `Void` element in Matroska, a `JUNK` chunk
in RIFF, with the bytes of the removed payload overwritten by zeros. The file
stays the same size, the media is untouched, and no stale metadata survives.

`core.CopyPatched` streams this so that multi-gigabyte files need almost no
memory. Handlers for potentially huge formats must stream and must not use
`core.ReadAll`.

### Hostile input rules

- Check every length and offset against the bounds of the file *before* using
  it, and guard against integer overflow when adding them.
- Cap recursion depth and element counts. Never allocate from an untrusted size
  without checking it first, and cap decompressed output (zip and flate bombs)
  with `core.ErrTooLarge`. An Office document may expand to 128 times the size of the
  file, between 32 MiB and 4 GiB, and to 1 GiB per entry; nested documents share the
  outermost file's allowance.
- Honour `ctx` in loops that can run long.
- Every handler ships a native Go fuzz test (`FuzzClean`) asserting: no panics,
  and if `Clean` succeeds then its output passes `Inspect` with no residual
  findings.

### Test data

Tests build fixtures in code. **No real photos or documents are committed**, and
fixtures use obviously fake values (`Test Author`, `Eiffel Tower` coordinates).

## Engine

The engine owns everything that is not format-specific.

- **Detection is by content**, never by extension. A renamed file is still
  handled correctly, and a file nothing recognises is reported as not cleaned.
- **Every input is accounted for.** Each file a walk or a wildcard finds gets a
  result. Two names count as one file only if the file system says so. On Windows,
  wildcards ignore case as they do everywhere else there, and a name no ordinary
  path can reach (ending in a dot or space) fails instead of being skipped quietly.
- **Atomic output.** Write to a temp file in the destination directory, fsync,
  verify, then rename. The temp file has mode 0600 on systems with mode bits; on
  Windows it has its folder's permissions until it is finished. Temp files are
  removed on any failure, including Ctrl-C. Only killing the process outright can
  leave one, and a later walk reports it as skipped.
- **Permissions.** The output gets the source's mode bits. On Windows it also gets
  any permissions the source has beyond what its folder hands down, so a private
  file stays private. If that fails, a warning says to check who can read it.
- **Verification before the output becomes visible:**
  1. Detect the temp file's format; it must equal the input's.
  2. Run `Inspect` on it. Any non-retained finding discards the output.
  3. Run the **residue scan**: a streaming search of the raw bytes for
     signatures such as `<x:xmpmeta`, `<?xpacket`, `ns.adobe.com/xap`, and
     `Photoshop 3.0`. It does not depend on any format parser, so it catches
     parser bugs, though it cannot see into compressed data. It fails unless
     `Format.ResidueWarnOnly` is set. A hit can be a false alarm when the visible
     content mentions the text, as with a PDF about XMP. The message says so and
     names the signature, but there is no override, because HideMe cannot tell the
     two cases apart.
- **Replacing an original.** `--in-place` keeps the name the file has on disk, even
  when it was named by its short name or in another case. If a handler sets
  `Report.MayLoseContent`, meaning the output may lack content the original holds,
  `--in-place` is refused and the original left alone unless `--force` is given.
  Writing to a separate output stays allowed, with the handler's warning. JPEG sets it
  for pictures or video after the end of the image, GIF for frames after damage it cut
  away, and TIFF for more bytes outside its tags than image inside them.
- **Dry runs** say what a real run with the same flags would do, refusals included:
  an output that already exists fails a dry run just as it fails the real one.
- **File-system metadata.** The output is a new file, so it inherits no NTFS
  alternate streams (`Zone.Identifier`, which can record the download URL) and
  no extended attributes (`com.apple.metadata:kMDItemWhereFroms`). `inspect`
  reports these where the platform allows. File times are set by `--timestamp`.
- **File names** are metadata too (`IMG_20240101_123456.jpg`). `--random-name`
  replaces them, and drops whatever follows the last dot unless it is an extension
  HideMe knows. Directory names mirrored beneath `-o` stay as they are.
- **Values stay private.** `clean` prints field names only, not the values it
  removed, so logs do not recreate the metadata. `-v` shows them. `inspect` prints
  values because that is its purpose; `--redact` hides them. Warnings do name the part
  they are about, such as a ZIP entry that was kept, because the user has to find it.
- **A handler's error message is never printed by default**, because it can quote the
  file. The engine wraps whatever `Clean` or `Inspect` returns where it receives it, and
  `Result.Reason` for such an error is a fixed message for its kind. The handler's words
  go in `Result.Detail`, which the CLI shows only where values are shown, so how a handler
  formats a message cannot decide whether it reaches a log. The engine's own errors keep
  their full text, because it holds nothing from the file.

## Known limits

These are stated to users in the README and in `hideme formats`.

- Text, faces and landmarks visible in the content itself are not metadata.
- Data inside encoded media streams (for example H.264 SEI messages or
  steganography) is not removed, because that would require re-encoding.
- `--in-place` replaces the directory entry. It cannot erase the old bytes from
  SSDs, snapshots, cloud sync or backups. Securely destroying originals is a
  separate problem and HideMe does not claim to solve it.
- Someone deliberately hiding data can still use bytes that readers ignore in encoded
  streams and fixed-format fields. `docs/SWEEP.md` measures this per format.
