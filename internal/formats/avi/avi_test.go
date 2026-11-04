package avi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/riff"
)

const (
	frameBytes = "VIDEO-FRAME-BYTES-MUST-SURVIVE"
	indexBytes = "INDEX-ENTRY"
	indxBytes  = "SUPER-INDX"
	formatText = "STRF-FMT"

	// padMark is planted in pad bytes. Chunk sizes in these small fixtures never
	// reach it, so finding it in the output means the pad byte survived.
	padMark = 0xEE
)

// chunk writes a chunk with a zero pad byte when its size is odd.
func chunk(id string, payloads ...[]byte) []byte {
	payload := slices.Concat(payloads...)
	out := slices.Concat([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(payload))), payload)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func list(listType string, children ...[]byte) []byte {
	return chunk("LIST", []byte(listType), slices.Concat(children...))
}

func riffChunk(form string, children ...[]byte) []byte {
	return chunk("RIFF", []byte(form), slices.Concat(children...))
}

func info() []byte {
	return list("INFO",
		chunk("ISFT", []byte("Test Software\x00")),
		chunk("IART", []byte("Test Author\x00")),
		chunk("ICMT", []byte("Test Comment\x00")),
		chunk("ICRD", []byte("2001-02-03\x00")),
		chunk("INAM", []byte("Test Title\x00")),
		chunk("IXYZ", []byte("Test Other Tag\x00")))
}

func movi() []byte {
	return list("movi", chunk("00dc", []byte(frameBytes)))
}

func streamList(extra ...[]byte) []byte {
	return list("strl",
		chunk("strh", []byte("vidsSTREAM-HEADER")),
		chunk("strf", []byte(formatText)),
		slices.Concat(extra...))
}

// videoProperties is a vprp chunk with one field description and a marker in its
// meaningful part.
func videoProperties() []byte {
	out := make([]byte, vprpHeaderSize+vprpFieldSize)
	copy(out, "VIDEO-PROPERTIES")
	binary.LittleEndian.PutUint32(out[vprpHeaderSize-4:], 1)
	return out
}

// fullFile has one of everything the handler removes, in the places real writers
// and camera firmware put them.
func fullFile() []byte {
	header := list("hdrl",
		chunk("avih", []byte("MAIN-AVI-HEADER")),
		streamList(
			chunk("strn", []byte("Test Stream Name\x00")),
			chunk("strd", []byte("Test Vendor Serial 12345")),
			chunk("vedt", []byte("Test Edit Data")),
			chunk("indx", []byte(indxBytes)),
			chunk("vprp", videoProperties()),
			chunk("JUNK", []byte("Test Banner In Junk")),
			chunk("zzzz", []byte("Test Unknown In Strl"))),
		list("odml", chunk("dmlh", []byte("DMLH")), chunk("zzzz", []byte("Test Unknown In Odml"))),
		chunk("IDIT", []byte("MON JAN 01 12:00:00 2001\n\x00")),
		chunk("ISMP", []byte("01:02:03:04\x00")),
		chunk("zzzz", []byte("Test Unknown In Hdrl")),
		list("zzzz", chunk("aaaa", []byte("Test Unknown List"))))
	first := riffChunk(formAVI,
		header,
		info(),
		chunk("JUNK", []byte("Test Stale Junk")),
		movi(),
		chunk("idx1", goodIndex()),
		chunk("zzzz", []byte("Test Unknown At Top")))
	second := riffChunk(formAVIX,
		chunk("JUNK", []byte("Test Stale Junk In AVIX")),
		movi(),
		chunk("ix00", []byte(indexBytes)),
		info())
	return slices.Concat(first, second, []byte("Test Trailing Secret"))
}

// minimalFile holds nothing to remove.
func minimalFile() []byte {
	return riffChunk(formAVI,
		list("hdrl", chunk("avih", []byte("MAIN-AVI-HEADER1")), streamList()),
		chunk("JUNK", make([]byte, 16)),
		movi(),
		chunk("idx1", goodIndex()))
}

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

func TestContract(t *testing.T) {
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name: "every kind of metadata", Data: fullFile(),
			WantFields: []string{
				"Software", "Artist", "Comment", "Creation date", "Title", "Other INFO entry",
				"Capture date", "Timecode", "Stream name", "Stream vendor data", "Video editing data",
				"Unknown chunk", "Hidden data in JUNK chunk", "Trailing data",
			},
			MustNotContain: asBytes(
				"Test Software", "Test Author", "Test Comment", "2001-02-03", "Test Title", "Test Other Tag",
				"INFO", "IDIT", "ISMP", "strn", "strd", "vedt", "zzzz",
				"MON JAN", "01:02:03:04", "Test Stream Name", "Test Vendor Serial", "Test Edit Data",
				"Test Banner", "Test Unknown", "Test Stale", "Test Trailing",
			),
			MustContain: asBytes(frameBytes, indexBytes, indxBytes, "MAIN-AVI-HEADER", "STREAM-HEADER", formatText, "VIDEO-PROPERTIES", "DMLH"),
		},
		{Name: "nothing to remove", Data: minimalFile(), MustContain: [][]byte{[]byte(frameBytes), goodIndex()}},
	})
}

func clean(t *testing.T, data []byte) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func wantError(t *testing.T, data []byte, want error) {
	t.Helper()
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, want) {
		t.Errorf("Inspect: got %v, want %v", err, want)
	}
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, want) {
		t.Errorf("Clean: got %v, want %v", err, want)
	}
}

// topLevel lists the output's RIFF chunks, failing on anything that does not
// parse. Comparing offsets against the input proves nothing moved.
func topLevel(t *testing.T, data []byte) []riff.Chunk {
	t.Helper()
	var chunks []riff.Chunk
	for c, err := range riff.Chunks(context.Background(), core.BytesSource(data), 0, int64(len(data))) {
		if err != nil {
			t.Fatalf("chunk walk: %v", err)
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// TestStructureIsPreserved is the content-preservation test. The standard library
// has no AVI decoder, but every chunk that stays must keep its bytes and offset.
func TestStructureIsPreserved(t *testing.T) {
	data := fullFile()
	out, _ := clean(t, data)
	trailing := len("Test Trailing Secret")
	if len(out) != len(data)-trailing {
		t.Fatalf("length went from %d to %d, want only the trailing bytes gone", len(data), len(out))
	}
	before := walkAll(t, data[:len(data)-trailing])
	after := walkAll(t, out)
	if len(before) != len(after) {
		t.Fatalf("chunk count changed from %d to %d", len(before), len(after))
	}
	for i, b := range before {
		a := after[i]
		if a.Offset != b.Offset || a.Size != b.Size || a.End != b.End {
			t.Errorf("chunk %d (%q) moved: %+v became %+v", i, b.ID, b, a)
		}
		switch a.ID {
		case "00dc", "idx1", "ix00", "indx", "avih", "strh", "strf", "vprp", "dmlh":
			if !bytes.Equal(data[b.DataOff:b.End], out[a.DataOff:a.End]) {
				t.Errorf("chunk %q changed", a.ID)
			}
		case "JUNK":
			if len(bytes.Trim(out[a.DataOff:a.End], "\x00")) != 0 {
				t.Errorf("JUNK chunk at %d is not zeroed", a.Offset)
			}
		}
	}
}

// walkAll flattens a file's chunks. It descends only into the lists a clean file
// can hold, so that a removed list counts as one chunk both before and after.
func walkAll(t *testing.T, data []byte) []riff.Chunk {
	t.Helper()
	src := core.BytesSource(data)
	var out []riff.Chunk
	var walk func(parent riff.Chunk)
	walk = func(parent riff.Chunk) {
		for c, err := range riff.Children(context.Background(), src, parent) {
			if err != nil {
				t.Fatalf("chunk walk: %v", err)
			}
			out = append(out, c)
			if c.ID != "LIST" {
				continue
			}
			kind, err := riff.ListType(src, c)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hdrl", "strl", "odml", "movi":
				walk(c)
			}
		}
	}
	for _, c := range topLevel(t, data) {
		out = append(out, c)
		walk(c)
	}
	return out
}

func TestPadBytes(t *testing.T) {
	// Odd-sized avih with a planted pad byte.
	avih := chunk("avih", []byte("ODD"))
	avih[len(avih)-1] = padMark
	file := riffChunk(formAVI, list("hdrl", avih, streamList()), movi())
	if file[8+4+8+4+8+3] != padMark {
		t.Fatal("the fixture's pad byte is not where the test expects")
	}
	out, _ := clean(t, file)
	if bytes.IndexByte(out, padMark) >= 0 {
		t.Error("a non-zero pad byte survived")
	}

	t.Run("an odd final chunk with no pad stays valid", func(t *testing.T) {
		noPad := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), movi(), chunk("ix00", []byte("odd")))
		noPad = noPad[:len(noPad)-1]
		binary.LittleEndian.PutUint32(noPad[4:], uint32(len(noPad)-8))
		if out, _ := clean(t, noPad); !bytes.Equal(out, noPad) {
			t.Error("the file changed")
		}
	})

	t.Run("a removed odd chunk loses its pad content too", func(t *testing.T) {
		removed := chunk("IDIT", []byte("ODD"))
		removed[len(removed)-1] = padMark
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList(), removed), movi())
		out, _ := clean(t, data)
		if bytes.IndexByte(out, padMark) >= 0 {
			t.Error("the pad byte of a removed chunk survived")
		}
	})

	t.Run("a pad byte after JUNK is zeroed", func(t *testing.T) {
		junk := chunk("JUNK", []byte{0, 0, 0})
		junk[len(junk)-1] = padMark
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), junk, movi())
		out, _ := clean(t, data)
		if bytes.IndexByte(out, padMark) >= 0 {
			t.Error("the pad byte after JUNK survived")
		}
	})
}

func TestTrailingBytes(t *testing.T) {
	file := minimalFile()
	for name, trailing := range map[string][]byte{
		"text":           []byte("Test Trailing Secret"),
		"one byte":       {padMark},
		"short RIFF":     []byte("RIFF"),
		"a WAVE":         riffChunk("WAVE", []byte("Test Wave")),
		"AVIX then junk": slices.Concat(riffChunk(formAVIX, movi()), []byte("Test Trailing")),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, slices.Concat(file, trailing))
			if bytes.Contains(out, []byte("Test")) {
				t.Errorf("trailing bytes survived: %q", out[len(file):])
			}
			if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Trailing data" }) {
				t.Errorf("no finding for trailing data: %+v", rep.Findings)
			}
		})
	}
	// A continuation segment that overruns the file is a damaged video.
	cut := riffChunk(formAVIX, movi())
	wantError(t, slices.Concat(file, cut[:len(cut)-4]), core.ErrMalformed)
}

func TestContinuationSegments(t *testing.T) {
	first := minimalFile()
	second := riffChunk(formAVIX, movi())
	third := riffChunk(formAVIX, movi(), info())
	data := slices.Concat(first, second, third)
	out, rep := clean(t, data)
	if len(out) != len(data) {
		t.Fatalf("length changed from %d to %d", len(data), len(out))
	}
	if n := bytes.Count(out, []byte(frameBytes)); n != 3 {
		t.Errorf("%d of 3 frames survived", n)
	}
	if bytes.Contains(out, []byte("Test Author")) || len(rep.Findings) == 0 {
		t.Errorf("INFO in the third segment was missed: %+v", rep.Findings)
	}
	if got := topLevel(t, out); len(got) != 3 {
		t.Errorf("%d RIFF chunks, want 3", len(got))
	}
}

func TestDamagedInfoStillGoes(t *testing.T) {
	broken := chunk("LIST", []byte("INFO"), []byte("ISFT\xFF\xFF\xFF\xFFTest Hidden"))
	data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), broken, movi())
	out, rep := clean(t, data)
	if bytes.Contains(out, []byte("Test Hidden")) || bytes.Contains(out, []byte("INFO")) {
		t.Error("a damaged INFO list survived")
	}
	if !slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "INFO list" }) {
		t.Errorf("no finding for the damaged list: %+v", rep.Findings)
	}
}

func TestDecodedValues(t *testing.T) {
	latin := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()),
		list("INFO", chunk("IART", []byte("Ren\xE9e\x00"))), movi())
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource(latin), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Value != "Renée" || rep.Findings[0].Risk != core.RiskHigh {
		t.Errorf("findings: %+v", rep.Findings)
	}
	if rep.Format != "avi" {
		t.Errorf("Format is %q", rep.Format)
	}
}

func TestRefusals(t *testing.T) {
	header := list("hdrl", chunk("avih", []byte("HH")), streamList())
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"not RIFF":               {[]byte("JUNKxxxxAVI xxxxxxxx"), core.ErrUnsupported},
		"RIFX":                   {[]byte("RIFX\x04\x00\x00\x00AVI "), core.ErrUnsupported},
		"other form":             {riffChunk("WAVE", header), core.ErrUnsupported},
		"too short":              {[]byte("RIFF"), core.ErrMalformed},
		"no movi":                {riffChunk(formAVI, header), core.ErrMalformed},
		"no hdrl":                {riffChunk(formAVI, movi()), core.ErrMalformed},
		"oversized avih":         {riffChunk(formAVI, list("hdrl", chunk("avih", make([]byte, maxHeaderChunk+2))), movi()), core.ErrMalformed},
		"no avih":                {riffChunk(formAVI, list("hdrl", streamList()), movi()), core.ErrMalformed},
		"overrunning chunk":      {riffChunk(formAVI, []byte("zzzz\xFF\x00\x00\x00")), core.ErrMalformed},
		"stray bytes":            {riffChunk(formAVI, header, movi(), []byte("abc")), core.ErrMalformed},
		"tiny LIST":              {riffChunk(formAVI, chunk("LIST", []byte("ab"))), core.ErrMalformed},
		"overrunning strl":       {riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), []byte("LIST\xFF\x00\x00\x00strl")), movi()), core.ErrMalformed},
		"RIFF shorter than form": {[]byte("RIFF\x02\x00\x00\x00AVI "), core.ErrMalformed},
	} {
		t.Run(name, func(t *testing.T) { wantError(t, tc.data, tc.want) })
	}
}

func TestMatch(t *testing.T) {
	match := func(data []byte) bool {
		return handler{}.Match(data[:min(len(data), core.HeadSize)], core.BytesSource(data))
	}
	if !match(minimalFile()) {
		t.Error("an AVI does not match")
	}
	for name, data := range map[string][]byte{
		"empty":        nil,
		"WAVE":         riffChunk("WAVE"),
		"WebP":         riffChunk("WEBP"),
		"continuation": riffChunk(formAVIX, movi()),
		"text":         []byte("not a video at all"),
	} {
		if match(data) {
			t.Errorf("%s matches", name)
		}
	}
}

func TestLimits(t *testing.T) {
	t.Run("the report stops growing", func(t *testing.T) {
		entries := bytes.Repeat(chunk("IART", []byte("Test")), maxFindings*3)
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), list("INFO", entries), movi())
		_, rep := clean(t, data)
		if len(rep.Findings) != maxFindings+1 {
			t.Errorf("%d findings", len(rep.Findings))
		}
		if last := rep.Findings[len(rep.Findings)-1]; last.Field != "Further metadata" {
			t.Errorf("the last finding is %+v", last)
		}
	})
	t.Run("too many rewrites", func(t *testing.T) {
		entries := bytes.Repeat(chunk("zzzz"), maxPatches+1)
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), entries), movi())
		wantError(t, data, core.ErrTooLarge)
	})
}

func TestStandardIndexNames(t *testing.T) {
	for id, want := range map[string]bool{
		"ix00": true, "ix0a": true, "00ix": true, "1Fix": true,
		"ixxx": false, "zzix": false, "ix": false, "idx1": false, "ix0": false,
	} {
		if got := isStandardIndex(id); got != want {
			t.Errorf("isStandardIndex(%q) = %v", id, got)
		}
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != "avi" || f.Family != core.FamilyVideo || !slices.Contains(f.Extensions, ".avi") || len(f.Limitations) == 0 || f.ResidueWarnOnly {
		t.Errorf("Format is %+v", f)
	}
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, fullFile())
}

func FuzzClean(f *testing.F) {
	f.Add(fullFile())
	f.Add(minimalFile())
	f.Add(slices.Concat(minimalFile(), riffChunk(formAVIX, movi(), info()), []byte("trailing")))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}
