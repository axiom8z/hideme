package riff

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"iter"
	"math"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// chunk returns a chunk with its pad byte when the payload is odd.
func chunk(id string, payload ...byte) []byte {
	out := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// listChunk returns a list with the given ID and type holding children.
func listChunk(id, typ string, children ...[]byte) []byte {
	return chunk(id, append([]byte(typ), bytes.Join(children, nil)...)...)
}

func riffFile(form string, children ...[]byte) []byte {
	return listChunk("RIFF", form, children...)
}

// rawHeader returns a chunk header declaring a size unrelated to what follows it.
func rawHeader(id string, size uint32) []byte {
	return append([]byte(id), binary.LittleEndian.AppendUint32(nil, size)...)
}

// patchRootSize makes a hand-built file's RIFF size match its contents.
func patchRootSize(file []byte) []byte {
	binary.LittleEndian.PutUint32(file[4:], uint32(len(file)-headerSize))
	return file
}

// walk collects a sequence and asserts the iterator's one promise about errors:
// nothing comes after the first.
func walk(t *testing.T, seq iter.Seq2[Chunk, error]) ([]Chunk, error) {
	t.Helper()
	var chunks []Chunk
	var first error
	for c, err := range seq {
		if first != nil {
			t.Errorf("the iterator continued after an error, yielding %+v, %v", c, err)
		}
		if err != nil {
			if c != (Chunk{}) {
				t.Errorf("an error came with a non-zero chunk: %+v", c)
			}
			first = err
			continue
		}
		chunks = append(chunks, c)
	}
	return chunks, first
}

func walkFile(t *testing.T, file []byte) ([]Chunk, error) {
	t.Helper()
	src := core.BytesSource(file)
	root, _, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return walk(t, Children(context.Background(), src, root))
}

func TestForm(t *testing.T) {
	tests := []struct {
		name string
		head string
		want string
	}{
		{"WebP", "RIFF\x10\x00\x00\x00WEBP", "WEBP"},
		{"WAV", "RIFF\x10\x00\x00\x00WAVEfmt ", "WAVE"},
		{"AVI, whose form has a space in it", "RIFF\x10\x00\x00\x00AVI LIST", "AVI "},
		{"sizes are not checked", "RIFF\xFF\xFF\xFF\xFFWEBP", "WEBP"},
		{"empty", "", ""},
		{"too short for a form", "RIFF\x10\x00\x00\x00WEB", ""},
		{"lower case is not RIFF", "riff\x10\x00\x00\x00WEBP", ""},
		{"big-endian variant", "RIFX\x00\x00\x00\x10WEBP", ""},
		{"64-bit variant", "RF64\xFF\xFF\xFF\xFFWAVE", ""},
		{"something else", "\xFF\xD8\xFF\xE0\x00\x10JFIF\x00\x01", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Form([]byte(tt.head)); got != tt.want {
				t.Errorf("Form(%q) = %q, want %q", tt.head, got, tt.want)
			}
		})
	}
}

func TestOpen(t *testing.T) {
	file := riffFile("WEBP", chunk("VP8 ", 1, 2))
	root, form, err := Open(core.BytesSource(file))
	if err != nil {
		t.Fatal(err)
	}
	want := Chunk{ID: "RIFF", Offset: 0, DataOff: 8, Size: 14, End: 22}
	if root != want {
		t.Errorf("root = %+v, want %+v", root, want)
	}
	if form != "WEBP" {
		t.Errorf("form = %q", form)
	}
	if !root.IsList() {
		t.Error("the root is not a list")
	}
}

func TestOpenLeavesTrailingBytesToTheHandler(t *testing.T) {
	file := riffFile("WAVE", chunk("data", 1, 2))
	end := int64(len(file))
	file = append(file, "secret trailer"...)
	root, _, err := Open(core.BytesSource(file))
	if err != nil {
		t.Fatal(err)
	}
	if root.End != end {
		t.Errorf("root.End = %d, want %d", root.End, end)
	}
	chunks, err := walk(t, Children(context.Background(), core.BytesSource(file), root))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].ID != "data" {
		t.Errorf("walked into the trailer: %+v", chunks)
	}
}

func TestOpenRejects(t *testing.T) {
	tests := []struct {
		name string
		file []byte
		want error
		text string
	}{
		{"empty", nil, core.ErrMalformed, "too short"},
		{"eleven bytes", []byte("RIFF\x04\x00\x00\x00WEB"), core.ErrMalformed, "too short"},
		{"big-endian RIFX", []byte("RIFX\x00\x00\x00\x04WEBP"), core.ErrUnsupported, "RIFX"},
		{"64-bit RF64", []byte("RF64\xFF\xFF\xFF\xFFWAVE"), core.ErrUnsupported, "RF64"},
		{"64-bit BW64", []byte("BW64\xFF\xFF\xFF\xFFWAVE"), core.ErrUnsupported, "BW64"},
		{"not RIFF at all", []byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00\x01"), core.ErrUnsupported, "not RIFF"},
		{"lower case", []byte("riff\x04\x00\x00\x00WEBP"), core.ErrUnsupported, "not RIFF"},
		{"size of zero leaves no room for a form", []byte("RIFF\x00\x00\x00\x00WEBP"), core.ErrMalformed, "no room"},
		{"size of three leaves no room for a form", []byte("RIFF\x03\x00\x00\x00WEBP"), core.ErrMalformed, "no room"},
		{"truncated file", []byte("RIFF\x10\x00\x00\x00WEBPxxxx"), core.ErrMalformed, "overrun"},
		{"maximum size", []byte("RIFF\xFF\xFF\xFF\xFFWAVE"), core.ErrMalformed, "overrun"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Open(core.BytesSource(tt.file))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.text) {
				t.Errorf("err = %q, want it to mention %q", err, tt.text)
			}
		})
	}
}

func TestChildrenReportsOffsetsAndSizes(t *testing.T) {
	file := riffFile("WEBP",
		chunk("VP8X", 1, 2, 3, 4),
		chunk("ALPH", 1, 2, 3),
		chunk("EXIF"),
		chunk("XMP ", 9, 9),
	)
	got, err := walkFile(t, file)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chunk{
		{ID: "VP8X", Offset: 12, DataOff: 20, Size: 4, End: 24},
		{ID: "ALPH", Offset: 24, DataOff: 32, Size: 3, End: 36}, // one pad byte
		{ID: "EXIF", Offset: 36, DataOff: 44, Size: 0, End: 44},
		{ID: "XMP ", Offset: 44, DataOff: 52, Size: 2, End: 54},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunk %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if payload := file[got[1].DataOff : got[1].DataOff+got[1].Size]; !bytes.Equal(payload, []byte{1, 2, 3}) {
		t.Errorf("ALPH payload = %v", payload)
	}
}

func TestPadBytes(t *testing.T) {
	t.Run("the pad after an odd chunk is not chunk data", func(t *testing.T) {
		// A nonzero pad must still be skipped rather than read as the next header.
		file := append(chunk("AAAA", 1), chunk("BBBB")...)
		file[8+1] = 0xEE
		chunks, err := walk(t, Chunks(context.Background(), core.BytesSource(file), 0, int64(len(file))))
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 2 || chunks[0].End != 10 || chunks[1].ID != "BBBB" {
			t.Errorf("chunks = %+v", chunks)
		}
	})
	t.Run("a pad at the parent's end is inside the parent", func(t *testing.T) {
		chunks, err := walkFile(t, riffFile("WAVE", chunk("data", 1, 2, 3)))
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].End != 24 || chunks[0].Size != 3 {
			t.Errorf("chunks = %+v", chunks)
		}
	})
	t.Run("a writer that omits the final pad is tolerated", func(t *testing.T) {
		file := patchRootSize(riffFile("WAVE", chunk("data", 1, 2, 3))[:23])
		chunks, err := walkFile(t, file)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 1 || chunks[0].End != 23 {
			t.Errorf("chunks = %+v", chunks)
		}
	})
	t.Run("a missing pad at a nested list's end does not spill into its sibling", func(t *testing.T) {
		inner := chunk("IART", 1, 2, 3)
		inner = inner[:len(inner)-1] // no pad, and an odd-sized list
		list := append(rawHeader("LIST", uint32(4+len(inner))), "INFO"...)
		list = append(list, inner...)
		list = append(list, 0) // the list's own pad
		file := patchRootSize(append(append([]byte("RIFF\x00\x00\x00\x00WAVE"), list...), chunk("data", 7)...))

		top, err := walkFile(t, file)
		if err != nil {
			t.Fatal(err)
		}
		if len(top) != 2 || top[1].ID != "data" {
			t.Fatalf("top level = %+v", top)
		}
		children, err := walk(t, Children(context.Background(), core.BytesSource(file), top[0]))
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 1 || children[0].End != top[0].DataOff+top[0].Size {
			t.Errorf("children = %+v, list = %+v", children, top[0])
		}
	})
	t.Run("an odd root does not extend past the file", func(t *testing.T) {
		file := patchRootSize(append([]byte("RIFF\x00\x00\x00\x00WAVE"), 'x'))
		file = file[:12]
		binary.LittleEndian.PutUint32(file[4:], 5)
		if _, _, err := Open(core.BytesSource(file)); !errors.Is(err, core.ErrMalformed) {
			t.Errorf("err = %v", err)
		}
		file = append(file, 'x')
		root, _, err := Open(core.BytesSource(file))
		if err != nil {
			t.Fatal(err)
		}
		if root.End != 13 {
			t.Errorf("root.End = %d", root.End)
		}
	})
}

func TestNestedLists(t *testing.T) {
	info := listChunk("LIST", "INFO", chunk("IART", 'T', 'e', 's', 't'), chunk("ICMT", 'x'))
	file := riffFile("WAVE", chunk("fmt ", 1, 2), info, chunk("data", 1, 2))
	src := core.BytesSource(file)
	top, err := walkFile(t, file)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || !top[1].IsList() || top[0].IsList() {
		t.Fatalf("top level = %+v", top)
	}
	typ, err := ListType(src, top[1])
	if err != nil || typ != "INFO" {
		t.Errorf("ListType = %q, %v", typ, err)
	}
	children, err := walk(t, Children(context.Background(), src, top[1]))
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].ID != "IART" || children[1].ID != "ICMT" {
		t.Errorf("children = %+v", children)
	}
	if got := file[children[0].DataOff : children[0].DataOff+children[0].Size]; string(got) != "Test" {
		t.Errorf("IART payload = %q", got)
	}
	if children[1].End != top[1].End {
		t.Errorf("the last child ends at %d, the list at %d", children[1].End, top[1].End)
	}
}

func TestListTypeOfTheRoot(t *testing.T) {
	src := core.BytesSource(riffFile("AVI "))
	root, _, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if typ, err := ListType(src, root); err != nil || typ != "AVI " {
		t.Errorf("ListType = %q, %v", typ, err)
	}
	chunks, err := walk(t, Children(context.Background(), src, root))
	if err != nil || len(chunks) != 0 {
		t.Errorf("an empty form gave %+v, %v", chunks, err)
	}
}

func TestIsList(t *testing.T) {
	for id, want := range map[string]bool{"LIST": true, "RIFF": true, "list": false, "JUNK": false, "": false, "RIFX": false} {
		if got := (Chunk{ID: id}).IsList(); got != want {
			t.Errorf("IsList(%q) = %v", id, got)
		}
	}
}

func TestNotAListIsAnError(t *testing.T) {
	src := core.BytesSource(riffFile("WAVE", chunk("data", 1, 2, 3, 4)))
	data := Chunk{ID: "data", Offset: 12, DataOff: 20, Size: 4, End: 24}
	if _, err := walk(t, Children(context.Background(), src, data)); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Children: err = %v", err)
	}
	if _, err := ListType(src, data); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("ListType: err = %v", err)
	}
	tiny := Chunk{ID: "LIST", Offset: 12, DataOff: 20, Size: 3, End: 24}
	if _, err := walk(t, Children(context.Background(), src, tiny)); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Children of a short list: err = %v", err)
	}
	if _, err := ListType(src, tiny); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("ListType of a short list: err = %v", err)
	}
}

// TestMalformedChildren builds each file by hand because the point of the test is
// a size that does not match the bytes. Every case keeps the root itself valid.
func TestMalformedChildren(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
		// wantChunks are the IDs yielded before the error.
		wantChunks int
	}{
		{"chunk overruns the root by one", append(rawHeader("data", 5), 1, 2, 3, 4), "overrunning its parent", 0},
		{"maximum chunk size", append(rawHeader("data", 0xFFFFFFFF), 1, 2, 3, 4), "overrunning its parent", 0},
		{"a good chunk and then a bad one", append(chunk("fmt ", 1, 2), append(rawHeader("data", 100), 1, 2)...), "overrunning its parent", 1},
		{"one stray byte", append(chunk("fmt ", 1, 2), 'x'), "stray", 1},
		{"seven stray bytes", append(chunk("fmt ", 1, 2), "1234567"...), "stray", 1},
		{"chunk header cut off", []byte("data\x04\x00"), "stray", 0},
		{"a pad byte followed by a stray byte", append(chunk("fmt ", 1), 'x'), "stray", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := patchRootSize(append([]byte("RIFF\x00\x00\x00\x00WAVE"), tt.body...))
			chunks, err := walkFile(t, file)
			if !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
			if len(chunks) != tt.wantChunks {
				t.Errorf("yielded %+v before the error, want %d chunks", chunks, tt.wantChunks)
			}
		})
	}
}

// TestSubchunkOverrunningItsListIsCaught is the case a walker checking only the
// file size would miss: the subchunk's bytes exist, but they belong to a sibling.
func TestSubchunkOverrunningItsListIsCaught(t *testing.T) {
	inner := append(rawHeader("IART", 20), 1, 2, 3, 4) // claims more than the list holds
	list := append(append(rawHeader("LIST", uint32(4+len(inner))), "INFO"...), inner...)
	file := riffFile("WAVE", list, chunk("data", make([]byte, 64)...))
	src := core.BytesSource(file)
	top, err := walkFile(t, file)
	if err != nil {
		t.Fatal(err)
	}
	_, err = walk(t, Children(context.Background(), src, top[0]))
	if !errors.Is(err, core.ErrMalformed) || !strings.Contains(err.Error(), "overrunning its parent") {
		t.Errorf("err = %v", err)
	}
}

func TestChunkOverrunningItsParentIsNamedSafely(t *testing.T) {
	file := append([]byte("RIFF\x00\x00\x00\x00WAVE"), append(rawHeader("\x1b[2J", 100), 1, 2)...)
	_, err := walkFile(t, patchRootSize(file))
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err = %q", err)
	}
}

func TestChunksRangeChecks(t *testing.T) {
	src := core.BytesSource(make([]byte, 16))
	tests := []struct {
		name       string
		start, end int64
	}{
		{"negative start", -1, 8},
		{"start after end", 8, 4},
		{"end past the file", 0, 17},
		{"both past the file", 100, 200},
		{"largest range", math.MinInt64, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := walk(t, Chunks(context.Background(), src, tt.start, tt.end)); !errors.Is(err, core.ErrMalformed) {
				t.Errorf("err = %v", err)
			}
		})
	}
	t.Run("empty range yields nothing", func(t *testing.T) {
		chunks, err := walk(t, Chunks(context.Background(), src, 8, 8))
		if err != nil || len(chunks) != 0 {
			t.Errorf("got %+v, %v", chunks, err)
		}
	})
}

// TestOpenDMLSegments walks the form AVI uses past 1 GiB: several RIFF chunks in
// a row, which is why Chunks is exported beside Children.
func TestOpenDMLSegments(t *testing.T) {
	first := riffFile("AVI ", chunk("idx1", 1, 2))
	second := riffFile("AVIX", chunk("JUNK", 3, 4))
	file := append(append([]byte{}, first...), second...)
	src := core.BytesSource(file)
	segments, err := walk(t, Chunks(context.Background(), src, 0, src.Size()))
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || !segments[1].IsList() || segments[1].Offset != int64(len(first)) {
		t.Fatalf("segments = %+v", segments)
	}
	typ, err := ListType(src, segments[1])
	if err != nil || typ != "AVIX" {
		t.Errorf("ListType = %q, %v", typ, err)
	}
	children, err := walk(t, Children(context.Background(), src, segments[1]))
	if err != nil || len(children) != 1 || children[0].ID != "JUNK" {
		t.Errorf("children = %+v, %v", children, err)
	}
}

func TestContextIsHonoured(t *testing.T) {
	file := riffFile("WAVE", chunk("aaaa"), chunk("bbbb"), chunk("cccc"))
	src := core.BytesSource(file)
	root, _, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		chunks, err := walk(t, Children(ctx, src, root))
		if !errors.Is(err, context.Canceled) || len(chunks) != 0 {
			t.Errorf("got %+v, %v", chunks, err)
		}
	})
	t.Run("cancelled part way", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		seen := 0
		var last error
		for _, err := range Children(ctx, src, root) {
			if err != nil {
				last = err
				continue
			}
			seen++
			cancel()
		}
		if seen != 1 || !errors.Is(last, context.Canceled) {
			t.Errorf("seen %d chunks, err = %v", seen, last)
		}
	})
}

func TestBreakingOutStopsReading(t *testing.T) {
	file := riffFile("WAVE", chunk("aaaa"), chunk("bbbb"), chunk("cccc"))
	src := &countingSource{Source: core.BytesSource(file)}
	root, _, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	src.reads = 0
	for range Children(context.Background(), src, root) {
		break
	}
	if src.reads != 1 {
		t.Errorf("%d reads after breaking out, want 1", src.reads)
	}
}

type countingSource struct {
	core.Source
	reads int
}

func (s *countingSource) ReadAt(p []byte, off int64) (int, error) {
	s.reads++
	return s.Source.ReadAt(p, off)
}

// sparseSource claims a large size and holds only a few real regions, reading as
// zeros elsewhere. It lets the tests use files of many gigabytes, and it records
// how much was read so they can prove payloads were left alone.
type sparseSource struct {
	size      int64
	regions   []region
	bytesRead int64
}

type region struct {
	off  int64
	data []byte
}

func (s *sparseSource) Size() int64 { return s.size }

func (s *sparseSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > s.size-off {
		n = int(s.size - off)
	}
	clear(p[:n])
	for _, r := range s.regions {
		if r.off >= off+int64(n) || off >= r.off+int64(len(r.data)) {
			continue
		}
		if r.off >= off {
			copy(p[r.off-off:n], r.data)
		} else {
			copy(p[:n], r.data[off-r.off:])
		}
	}
	s.bytesRead += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestHugeFileIsWalkedWithoutReadingPayloads uses offsets well past 4 GiB, as an
// OpenDML AVI has, to prove nothing is held in a 32-bit value and that a payload
// is never touched.
func TestHugeFileIsWalkedWithoutReadingPayloads(t *testing.T) {
	const big = int64(0xFFFFFFF0)
	const segment = headerSize + big
	src := &sparseSource{size: 2 * segment}
	src.regions = []region{
		{0, rawHeader("RIFF", uint32(big))},
		{8, []byte("AVI ")},
		{12, rawHeader("movi", uint32(big-formSize-headerSize))},
		{segment, rawHeader("RIFF", uint32(big))},
		{segment + 8, []byte("AVIX")},
		{segment + 12, rawHeader("data", uint32(big-formSize-headerSize))},
	}
	segments, err := walk(t, Chunks(context.Background(), src, 0, src.size))
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[1].Offset != segment || segments[1].End != 2*segment {
		t.Fatalf("segments = %+v", segments)
	}
	if want := 2 * int64(headerSize); src.bytesRead != want {
		t.Errorf("read %d bytes to walk two segments, want %d", src.bytesRead, want)
	}

	children, err := walk(t, Children(context.Background(), src, segments[1]))
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != "data" || children[0].Offset != segment+12 {
		t.Errorf("children = %+v", children)
	}
}

func TestAllocationDoesNotDependOnChunkSize(t *testing.T) {
	allocs := func(payload int64) float64 {
		size := int64(formSize+headerSize) + payload
		src := &sparseSource{size: headerSize + size, regions: []region{
			{0, rawHeader("RIFF", uint32(size))},
			{8, []byte("WAVE")},
			{12, rawHeader("data", uint32(payload))},
		}}
		return testing.AllocsPerRun(20, func() {
			root, _, err := Open(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range Children(context.Background(), src, root) {
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	small, huge := allocs(16), allocs(0xFFFFFFF0-formSize-headerSize)
	if small != huge {
		t.Errorf("a 16-byte chunk costs %v allocations but a 4 GiB one costs %v", small, huge)
	}
}

// TestOffsetsNearTheLargestSizeDoNotOverflow gives a chunk a header close enough
// to the largest int64 that adding its size wraps negative, which would make it
// pass an overrun check and send the walk backwards.
func TestOffsetsNearTheLargestSizeDoNotOverflow(t *testing.T) {
	const size = int64(math.MaxInt64)
	off := size - 2*headerSize
	src := &sparseSource{size: size, regions: []region{{off, rawHeader("data", 0xFFFFFFFF)}}}
	chunks, err := walk(t, Chunks(context.Background(), src, off, size))
	if !errors.Is(err, core.ErrMalformed) || len(chunks) != 0 {
		t.Errorf("got %+v, %v", chunks, err)
	}
}

func TestEvenChunkEndingAtTheLargestOffset(t *testing.T) {
	const size = int64(math.MaxInt64)
	off := size - 2*headerSize
	src := &sparseSource{size: size, regions: []region{{off, rawHeader("data", headerSize)}}}
	chunks, err := walk(t, Chunks(context.Background(), src, off, size))
	if err != nil || len(chunks) != 1 || chunks[0].End != size {
		t.Errorf("got %+v, %v", chunks, err)
	}
}

func TestOddChunkEndingAtTheLargestOffset(t *testing.T) {
	const size = int64(math.MaxInt64)
	off := size - 2*headerSize + 1
	src := &sparseSource{size: size, regions: []region{{off, rawHeader("data", headerSize-1)}}}
	chunks, err := walk(t, Chunks(context.Background(), src, off, size))
	if err != nil || len(chunks) != 1 || chunks[0].End != size {
		t.Errorf("got %+v, %v", chunks, err)
	}
}

// fixedSource returns a canned result from every read, for the failures a real
// disk can produce and the shapes io.ReaderAt allows.
type fixedSource struct {
	size int64
	data []byte
	err  error
}

func (s fixedSource) Size() int64 { return s.size }

func (s fixedSource) ReadAt(p []byte, _ int64) (int, error) {
	return copy(p, s.data), s.err
}

func TestReadFailures(t *testing.T) {
	errDisk := errors.New("disk failure")
	header := []byte("RIFF\x04\x00\x00\x00WAVE")

	t.Run("Open reports the failure", func(t *testing.T) {
		if _, _, err := Open(fixedSource{size: 12, err: errDisk}); !errors.Is(err, errDisk) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a short read with no error is still a failure", func(t *testing.T) {
		if _, _, err := Open(fixedSource{size: 12, data: header[:4]}); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a short read with EOF is a failure", func(t *testing.T) {
		if _, _, err := Open(fixedSource{size: 12, data: header[:4], err: io.EOF}); !errors.Is(err, io.EOF) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a full read with EOF is success", func(t *testing.T) {
		if _, form, err := Open(fixedSource{size: 12, data: header, err: io.EOF}); err != nil || form != "WAVE" {
			t.Errorf("form = %q, err = %v", form, err)
		}
	})
	t.Run("Chunks reports the failure", func(t *testing.T) {
		_, err := walk(t, Chunks(context.Background(), fixedSource{size: 32, err: errDisk}, 0, 32))
		if !errors.Is(err, errDisk) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("ListType reports the failure", func(t *testing.T) {
		list := Chunk{ID: "LIST", DataOff: 8, Size: 8, End: 16}
		if _, err := ListType(fixedSource{size: 16, err: errDisk}, list); !errors.Is(err, errDisk) {
			t.Errorf("err = %v", err)
		}
	})
}

// TestEveryTruncationAndCorruptionIsSafe sweeps a file with every structural
// feature through the same checks as the fuzz target.
func TestEveryTruncationAndCorruptionIsSafe(t *testing.T) {
	file := sampleFile()
	for n := range len(file) + 1 {
		checkFile(t, file[:n])
	}
	mutated := bytes.Clone(file)
	for i := range file {
		for _, value := range []byte{0x00, 0x01, 0x7F, 0x80, 0xFF, file[i] ^ 0xFF} {
			mutated[i] = value
			checkFile(t, mutated)
		}
		mutated[i] = file[i]
		if t.Failed() {
			t.Fatalf("corrupting byte %d broke an invariant", i)
		}
	}
}

func sampleFile() []byte {
	return riffFile("WAVE",
		chunk("fmt ", 1, 2, 3, 4),
		listChunk("LIST", "INFO", chunk("IART", 'T', 'e', 's', 't'), chunk("ICMT", 'o', 'd', 'd')),
		listChunk("LIST", "adtl", listChunk("LIST", "deep", chunk("deep", 1))),
		chunk("JUNK"),
		chunk("data", 1, 2, 3),
	)
}

// Errors reach the user, and a chunk's name is text from the file. clean promises not
// to repeat any of that.
func TestErrorsDoNotRepeatTextFromTheFile(t *testing.T) {
	const marker = "MRKR"
	overrun := patchRootSize(append([]byte("RIFF\x00\x00\x00\x00WAVE"), append(rawHeader(marker, 100), 1, 2)...))
	_, err := walkFile(t, overrun)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("overrun: err = %q", err)
	}
	if _, _, err := Open(core.BytesSource([]byte(marker + "\x00\x00\x00\x00WAVE"))); err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("open: err = %q", err)
	}
	_, err = walk(t, Children(context.Background(), core.BytesSource(make([]byte, 16)), Chunk{ID: marker, Size: 8}))
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("not a list: err = %q", err)
	}
}
