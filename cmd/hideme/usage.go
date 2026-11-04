package main

const exitCodesHelp = `Exit codes:
  0  every file was processed and, for clean, written and verified
  1  at least one file failed; its original is untouched and no output was left
  2  the command line was wrong; nothing was done
  3  finished, but some files were unsupported or skipped, so they were not cleaned
`

const usageMain = `hideme removes privacy-sensitive metadata from files, without re-encoding them.

Usage:
  hideme inspect [flags] PATH...   list the metadata in files, changing nothing
  hideme clean [flags] PATH...     write metadata-free copies
  hideme formats [--json]          list supported formats and what they cannot remove
  hideme version                   print the version
  hideme help [TOPIC]              help on inspect, clean, formats, version or json

Files are recognised by their content, never by their name. A cleaned file is only
given its name after it has been verified, and originals are never touched unless
you ask for that with --in-place.

` + exitCodesHelp

const usageInspect = `Usage: hideme inspect [flags] PATH...

Lists the metadata in each file: grouped by risk with the value and where it was
found, then what cleaning would deliberately keep, then warnings about what it cannot
clean. Nothing is changed. File-system metadata is included where the platform allows,
such as the download URL Windows records in the Zone.Identifier stream.

Flags may come before or after paths. A path starting with "-" goes after "--".

  --json      print one JSON document; see "hideme help json"
  --redact    hide values, showing only what kind of metadata was found
  --strict    show what clean --strict would do
  -r          walk directories (symbolic links are not followed)
  --jobs N    process N files at once (default: the CPU count, at most 8)

` + exitCodesHelp

const usageClean = `Usage: hideme clean [flags] PATH...

Writes a metadata-free copy of each file. By default photo.jpg gets a neighbour called
photo.clean.jpg. Each output is written to a temporary file, synced, and checked before
it gets its name: its format must be unchanged, reading it back must find no metadata,
and a search of its raw bytes must find no metadata signatures. If any check fails, the
output is discarded and the original kept. There is no override: a document whose
visible text mentions such a signature, like a PDF about XMP, is refused as well. A file
with nothing to remove is still rewritten and verified, and reported as "nothing found".

Only field names and counts are printed, so that logs do not recreate what was removed.
Warnings are always printed: they say what HideMe could not clean.

Flags may come before or after paths. A path starting with "-" goes after "--".

  --json         print one JSON document; see "hideme help json"
  --strict       also remove orientation and ICC colour profiles, which are otherwise
                 kept because removing them visibly changes the picture
  -r             walk directories; symbolic links, special files and earlier outputs
                 are skipped, and those and any unsupported files are listed
  --jobs N       process N files at once (default: the CPU count, at most 8)
  -o DIR         write beneath DIR with the original names: a file goes in DIR, and a
                 walked directory in DIR/NAME with its structure kept
  --in-place     replace each original by atomic rename. This replaces a directory entry;
                 it cannot erase the old bytes from SSDs, snapshots, sync or backups.
                 Refused, leaving the original as it is, when the cleaned copy may lack
                 content the original holds, such as a TIFF stack whose later frames no
                 tag refers to. Write a copy instead and check it, or use --force
  --suffix S     insert S before the extension (default: .clean next to the source,
                 nothing with -o)
  --force        replace an output that already exists; with --in-place, also replace an
                 original whose cleaned copy may lack content
  --random-name  use a random name, since file names such as IMG_20240101_123456.jpg are
                 metadata too. A suffix stays, so that a later walk can still tell the file
                 is an output, and so does an extension HideMe knows; anything else after
                 the last dot, as in Scan.2024-03-15, goes with the rest of the name
  --timestamp T  file times of the output: now (default), zero (1980-01-01), or keep,
                 which copies the source's and so retains its capture time
  --dry-run      do everything except keeping the output, and report what a real run with
                 the same flags would do. That includes failing, with exit code 1, on an
                 output that already exists or an --in-place that would be refused
  -v             also print the removed values, and why a file failed in the format
                 handler's own words, which can quote the file. Keep this out of shared logs

` + exitCodesHelp

const usageFormats = `Usage: hideme formats [--json]

Lists each supported format, its extensions, and what it cannot remove.

` + exitCodesHelp

const usageVersion = `Usage: hideme version

Prints the version, the Go version and the platform.
`

const usageJSON = `With --json, stdout holds exactly one JSON document and nothing else, unless --help
was asked for too. Messages for people go to stderr. This is schema "hideme/1"; fields are
only ever added to it.

inspect and clean:
  {
    "schema": "hideme/1",
    "command": "inspect" or "clean",
    "dry_run": true or false,
    "results": [ one object per input, in input order, as below ],
    "summary": {
      "files", "inspected", "cleaned", "would_clean", "failed", "unsupported",
      "skipped", "warnings", "bytes_before", "bytes_after", "elapsed_ms"
    },
    "exit_code": the process's exit code,
    "error": present only for a usage error, with "results" empty
  }

Each result:
  {
    "path": the input, made safe for display (see below),
    "output": present once an output was written, or for a dry run would be,
    "status": "inspected", "cleaned", "would-clean", "failed", "unsupported" or "skipped",
    "format": the detected format,
    "nothing_found": true when a file was processed and held no metadata,
    "bytes_before", "bytes_after", "elapsed_ms": numbers,
    "findings": [ {"category", "risk", "field", "value", "where", "action", "reason"} ],
    "warnings": [ strings saying what HideMe could not clean ],
    "may_lose_content": true when the output may lack content the original holds, so that
      the original should be kept until the output has been checked,
    "reason": why the file failed, was skipped or is unsupported,
    "error_kind": a stable name for that reason, such as "malformed", "encrypted",
      "too-large", "unsupported", "verification", "output-exists", "same-file",
      "output-collision", "may-lose-content", "not-found", "invalid-input", "canceled",
      "internal", "io",
      "symlink", "special-file", "has-output-suffix", "output-directory", "temp-file"
  }

"findings" and "warnings" are always arrays. "value" is left out for clean unless -v is
given, and for inspect --redact, and in those cases a "reason" from a format handler is a
fixed message for its "error_kind", because the handler's own words can quote the file.
Paths and all other text are passed through a filter that drops control and
bidirectional-override characters, and a warning is added when that changed a file's name,
so a "path" is for display and may not match the disk.

formats:
  {
    "schema": "hideme/1",
    "command": "formats",
    "formats": [ {"id", "name", "family", "extensions", "limitations"} ],
    "filesystem": what this platform can report about file-system metadata
  }
`

var helpTopics = map[string]string{
	"inspect": usageInspect,
	"clean":   usageClean,
	"formats": usageFormats,
	"version": usageVersion,
	"json":    usageJSON,
}
