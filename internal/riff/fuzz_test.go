package riff

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// maxWalkDepth is a test's own cap, as the package documentation tells handlers
// to have, on how far it follows hostile nesting.
const maxWalkDepth = 32

func FuzzChunks(f *testing.F) {
	f.Add(sampleFile())
	f.Add(riffFile("WEBP", chunk("VP8X", 1, 2, 3, 4), chunk("EXIF", 'x')))
	f.Add(append(riffFile("AVI "), riffFile("AVIX", chunk("JUNK"))...))
	f.Add(append(rawHeader("RIFF", 0xFFFFFFFF), "WAVE"...))
	f.Add(append(append([]byte("RIFF\x10\x00\x00\x00WAVE"), rawHeader("LIST", 0xFFFFFFFF)...), "INFO"...))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVE"))
	f.Add([]byte("RF64\xFF\xFF\xFF\xFFWAVE"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		checkFile(t, data)
	})
}

// checkFile asserts what must hold for any bytes at all: walking terminates,
// every chunk lies inside its parent, and every error is one the engine can
// classify. Termination is proved by the test finishing.
func checkFile(t *testing.T, data []byte) {
	t.Helper()
	src := core.BytesSource(data)
	root, form, err := Open(src)
	if err != nil {
		checkError(t, err)
	} else {
		if form != Form(data) {
			t.Errorf("Open says form %q but Form says %q", form, Form(data))
		}
		if root.DataOff != headerSize || root.Size < formSize || root.Offset != 0 {
			t.Errorf("root = %+v", root)
		}
		checkChunk(t, data, root, int64(len(data)))
		checkWalk(t, data, src, root, 0)
	}
	// The flat walk is what an OpenDML AVI handler uses, over the whole file.
	checkSequence(t, data, src, 0, int64(len(data)), 0)
}

func checkWalk(t *testing.T, data []byte, src core.Source, parent Chunk, depth int) {
	t.Helper()
	typ, err := ListType(src, parent)
	if err != nil {
		checkError(t, err)
		return
	}
	if typ != string(data[parent.DataOff:parent.DataOff+formSize]) {
		t.Errorf("ListType = %q", typ)
	}
	checkSequence(t, data, src, parent.DataOff+formSize, parent.DataOff+parent.Size, depth)
}

func checkSequence(t *testing.T, data []byte, src core.Source, start, end int64, depth int) {
	t.Helper()
	pos := start
	for c, err := range Chunks(context.Background(), src, start, end) {
		if err != nil {
			checkError(t, err)
			return
		}
		if c.Offset != pos {
			t.Errorf("chunk at %d follows a chunk ending at %d", c.Offset, pos)
		}
		checkChunk(t, data, c, end)
		if c.End <= pos {
			t.Fatalf("the walk did not advance past %d: %+v", pos, c)
		}
		pos = c.End
		if c.IsList() && c.Size >= formSize && depth < maxWalkDepth {
			checkWalk(t, data, src, c, depth+1)
		}
	}
	if pos != end {
		t.Errorf("a clean walk of %d to %d stopped at %d", start, end, pos)
	}
}

func checkChunk(t *testing.T, data []byte, c Chunk, parentEnd int64) {
	t.Helper()
	if c.DataOff != c.Offset+headerSize || c.Offset < 0 || c.Size < 0 {
		t.Errorf("inconsistent chunk: %+v", c)
		return
	}
	payloadEnd := c.DataOff + c.Size
	if payloadEnd > parentEnd || c.End > parentEnd {
		t.Errorf("chunk %+v overruns a parent ending at %d", c, parentEnd)
		return
	}
	wantEnd := payloadEnd
	if c.Size%2 == 1 && payloadEnd < parentEnd {
		wantEnd++
	}
	if c.End != wantEnd {
		t.Errorf("chunk %+v should end at %d", c, wantEnd)
	}
	if got := string(data[c.Offset : c.Offset+4]); got != c.ID {
		t.Errorf("ID = %q, the file says %q", c.ID, got)
	}
	if got := int64(binary.LittleEndian.Uint32(data[c.Offset+4:])); got != c.Size {
		t.Errorf("Size = %d, the file says %d", c.Size, got)
	}
}

func checkError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, core.ErrMalformed) && !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("error wraps no core sentinel: %v", err)
	}
}
