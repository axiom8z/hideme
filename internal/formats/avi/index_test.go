package avi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

func hasFinding(findings []core.Finding, field string) bool {
	return slices.ContainsFunc(findings, func(f core.Finding) bool { return f.Field == field })
}

// indexEntry writes one idx1 entry.
func indexEntry(id string, flags, offset, size uint32) []byte {
	out := []byte(id)
	out = binary.LittleEndian.AppendUint32(out, flags)
	out = binary.LittleEndian.AppendUint32(out, offset)
	return binary.LittleEndian.AppendUint32(out, size)
}

// movieWithIndex is a file whose index follows its one frame.
func movieWithIndex(index []byte) []byte {
	return riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), movi(), chunk("idx1", index))
}

// goodIndex points at the frame in movi(), relative to the movi list as most writers do.
func goodIndex() []byte {
	return indexEntry("00dc", 0x10, 4, uint32(len(frameBytes)))
}

// An index can correctly list a chunk that cleaning removes. Checked only against the
// original it matched, so it stayed, still naming a chunk the output no longer has, and a
// second clean removed it. Fuzzing found this.
func TestIndexListingARemovedChunkGoesToo(t *testing.T) {
	const strayBody = "Test Stray Chunk"
	data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()),
		list("movi", chunk("Tst!", []byte(strayBody)), chunk("00dc", []byte(frameBytes))),
		chunk("idx1", indexEntry("Tst!", 0x10, 4, uint32(len(strayBody)))))
	out, rep := clean(t, data)
	if bytes.Contains(out, []byte("Tst!")) || bytes.Contains(out, []byte(strayBody)) {
		t.Fatal("the stray chunk's name or bytes survived")
	}
	if !hasFinding(rep.Findings, "Index that does not match the file") {
		t.Fatalf("no finding about the index: %+v", rep.Findings)
	}
	if !bytes.Contains(out, []byte(frameBytes)) {
		t.Fatal("the frame was lost")
	}
	if again, _ := clean(t, out); !bytes.Equal(again, out) {
		t.Fatal("a second clean changed the file")
	}
}

// An index is only ever read as a list of places in the file, so what makes one real is
// that each entry describes the chunk that is there. Nothing else would ever check it,
// so without this it can hold anything at 16 bytes an entry, in as many entries as a
// writer likes, and only text of 12 characters or more ever raised a warning.
func TestIndexIsCheckedAgainstTheFile(t *testing.T) {
	file := movieWithIndex(goodIndex())
	movieAt := int64(bytes.Index(file, []byte("movi")))
	absolute := indexEntry("00dc", 0x10, uint32(movieAt)+4, uint32(len(frameBytes)))

	t.Run("a true index stays", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"relative": file,
			"absolute": movieWithIndex(absolute),
			"empty":    movieWithIndex(nil),
		} {
			t.Run(name, func(t *testing.T) {
				out, rep := clean(t, data)
				if !bytes.Equal(out, data) || len(rep.Warnings) != 0 || len(rep.Findings) != 0 {
					t.Fatalf("a true index changed: %q %+v", rep.Warnings, rep.Findings)
				}
			})
		}
	})
	for name, index := range map[string][]byte{
		"wrong ID":              indexEntry("Tst!", 0x10, 4, uint32(len(frameBytes))),
		"wrong offset":          indexEntry("00dc", 0x10, 0x54737421, uint32(len(frameBytes))),
		"wrong size":            indexEntry("00dc", 0x10, 4, 0x54737421),
		"flags nothing defines": indexEntry("00dc", 0x54737421, 4, uint32(len(frameBytes))),
		"a later entry":         slices.Concat(goodIndex(), indexEntry("Tst!", 0x10, 0x54737421, 0)),
		"not an index at all":   []byte("Test Index That Is Not One"),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, movieWithIndex(index))
			if bytes.Contains(out, []byte("Tst!")) || bytes.Contains(out, []byte("!tsT")) || bytes.Contains(out, []byte("Test Index")) {
				t.Fatal("the planted bytes survived")
			}
			if !hasFinding(rep.Findings, "Index that does not match the file") {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "index") {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
			if again, _ := clean(t, out); !bytes.Equal(again, out) {
				t.Fatal("a second clean changed the file")
			}
			if !bytes.Contains(out, []byte(frameBytes)) {
				t.Fatal("the frame was lost")
			}
		})
	}
	t.Run("bytes that make no whole entry", func(t *testing.T) {
		data := movieWithIndex(slices.Concat(goodIndex(), []byte("Tst!Tst")))
		out, rep := clean(t, data)
		if bytes.Contains(out, []byte("Tst!")) {
			t.Fatal("the bytes after the last entry survived")
		}
		if !bytes.Contains(out, goodIndex()) {
			t.Fatal("the index itself changed")
		}
		if len(rep.Warnings) != 0 {
			t.Fatalf("warnings = %q", rep.Warnings)
		}
		if len(out) != len(data) {
			t.Fatalf("length changed from %d to %d", len(data), len(out))
		}
	})
	t.Run("an old interleaved file lists its record lists", func(t *testing.T) {
		rec := list("rec ", chunk("00dc", []byte(frameBytes)))
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), list("movi", rec),
			chunk("idx1", slices.Concat(indexEntry("rec ", 0x01, 4, uint32(len(rec)-8)), indexEntry("00dc", 0x10, 16, uint32(len(frameBytes))))))
		out, rep := clean(t, data)
		if !bytes.Equal(out, data) || len(rep.Warnings) != 0 {
			t.Fatalf("a true index changed: %q", rep.Warnings)
		}
	})
}

// A WAVEFORMATEX says how long it is, so unlike a video format, what follows it is spare.
func TestBytesPastAnAudioFormatAreZeroed(t *testing.T) {
	format := func(extra int, tail string) []byte {
		out := make([]byte, 18)
		out[0], out[2] = 1, 2
		binary.LittleEndian.PutUint16(out[16:], uint16(extra))
		out = append(out, make([]byte, extra)...)
		return append(out, tail...)
	}
	file := func(strf []byte) []byte {
		return riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")),
			list("strl", chunk("strh", []byte("audsSTREAM-HEADER")), chunk("strf", strf))),
			list("movi", chunk("00wb", []byte(frameBytes))))
	}
	for name, strf := range map[string][]byte{
		"no extra bytes": format(0, "Tst!Spar"),
		"extra bytes":    format(4, "Tst!Spar"),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, file(strf))
			if bytes.Contains(out, []byte("Tst!")) {
				t.Fatal("the bytes past the format survived")
			}
			if !hasFinding(rep.Findings, "Hidden data in a header chunk") {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
		})
	}
	t.Run("a format too short to state its size loses nothing", func(t *testing.T) {
		out, _ := clean(t, file(slices.Concat(make([]byte, 14), []byte("Ts"))))
		if !bytes.Contains(out, []byte("Ts")) {
			t.Fatal("a format with no size to go by lost bytes")
		}
	})
	t.Run("a real format stays", func(t *testing.T) {
		data := file(format(4, ""))
		if out, rep := clean(t, data); !bytes.Equal(out, data) || len(rep.Findings) != 0 {
			t.Fatalf("a real format changed: %+v", rep.Findings)
		}
	})
}
