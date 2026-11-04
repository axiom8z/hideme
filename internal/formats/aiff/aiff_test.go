package aiff

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/axiom8z/hideme/internal/audiotag/tagtest"
	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

func chunkOf(id string, body []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte(id), uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func formFile(form string, chunks ...[]byte) []byte {
	body := append([]byte(form), bytes.Join(chunks, nil)...)
	out := binary.BigEndian.AppendUint32([]byte("FORM"), uint32(len(body)))
	return append(out, body...)
}

// comm describes mono 16-bit sound. The rate is 44.1 kHz in the 80-bit float AIFF uses.
func comm(frames uint32) []byte {
	out := binary.BigEndian.AppendUint16(nil, 1)
	out = binary.BigEndian.AppendUint32(out, frames)
	out = binary.BigEndian.AppendUint16(out, 16)
	return append(out, 0x40, 0x0E, 0xAC, 0x44, 0, 0, 0, 0, 0, 0)
}

// commC adds the AIFF-C compression type and its name as a Pascal string.
func commC(frames uint32) []byte {
	return append(comm(frames), "NONE\x0Enot compressed\x00"...)
}

func sound(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i * 7)
	}
	return out
}

func ssnd(samples []byte) []byte {
	return append(make([]byte, ssndHeaderLen), samples...)
}

// comt builds a COMT chunk body holding one comment.
func comt(text string) []byte {
	out := binary.BigEndian.AppendUint16(nil, 1)
	out = binary.BigEndian.AppendUint32(out, 0) // timestamp
	out = binary.BigEndian.AppendUint16(out, 0) // marker
	out = binary.BigEndian.AppendUint16(out, uint16(len(text)))
	out = append(out, text...)
	if len(text)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// mark builds a MARK chunk body holding the given marker names.
func mark(names ...string) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(len(names)))
	for i, name := range names {
		out = binary.BigEndian.AppendUint16(out, uint16(i+1))
		out = binary.BigEndian.AppendUint32(out, 0)
		out = append(out, byte(len(name)))
		out = append(out, name...)
		if (len(name)+1)%2 == 1 {
			out = append(out, 0)
		}
	}
	return out
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func clean(t *testing.T, data []byte) ([]byte, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource(data), &out, core.Options{})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	return out.Bytes(), rep
}

func values(rep *core.Report, field string) []string {
	var out []string
	for _, f := range rep.Findings {
		if f.Field == field {
			out = append(out, f.Value)
		}
	}
	return out
}

func TestContract(t *testing.T) {
	pcm := sound(1000)
	commChunk := chunkOf(idCOMM, comm(500))
	ssndChunk := chunkOf(idSSND, ssnd(pcm))

	cases := []coretest.Case{
		{Name: "no metadata", Data: formFile("AIFF", commChunk, ssndChunk), MustContain: [][]byte{comm(500), pcm}},
		{
			Name:        "AIFF-C with FVER",
			Data:        formFile("AIFC", chunkOf(idFVER, []byte{0xA2, 0x80, 0x51, 0x40}), chunkOf(idCOMM, commC(500)), ssndChunk),
			WantFields:  []string{"Compression name"},
			MustContain: [][]byte{commEmptyName(500), {0xA2, 0x80, 0x51, 0x40}, pcm},
		},
		{
			Name: "text chunks",
			Data: formFile("AIFF", chunkOf("NAME", []byte("Test Title")), chunkOf("AUTH", []byte(tagtest.Fake)),
				chunkOf("(c) ", []byte("Test Copyright")), chunkOf("ANNO", []byte("Test Annotation")), commChunk, ssndChunk),
			WantFields: []string{"Title", "Author", "Copyright", "Annotation"},
			MustNotContain: [][]byte{
				[]byte("NAME"), []byte("AUTH"), []byte("(c) "), []byte("ANNO"), []byte("Test Title"),
				[]byte(tagtest.Fake), []byte("Test Copyright"), []byte("Test Annotation"),
			},
			MustContain: [][]byte{pcm},
		},
		{
			Name:           "comments and markers",
			Data:           formFile("AIFF", commChunk, chunkOf("COMT", comt("Test Comment")), chunkOf("MARK", mark("Test Marker", "Test Second")), ssndChunk),
			WantFields:     []string{"Comment", "Marker"},
			MustNotContain: [][]byte{[]byte("COMT"), []byte("MARK"), []byte("Test Comment"), []byte("Test Marker"), []byte("Test Second")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "instrument and application data",
			Data:           formFile("AIFF", commChunk, chunkOf("INST", make([]byte, 20)), chunkOf("APPL", []byte("TEST Test Application")), ssndChunk),
			WantFields:     []string{"Instrument data", "Application data"},
			MustNotContain: [][]byte{[]byte("INST"), []byte("APPL"), []byte("Test Application")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "ID3 chunk",
			Data:           formFile("AIFF", commChunk, ssndChunk, chunkOf("ID3 ", tagtest.ID3v23())),
			WantFields:     tagtest.ID3Fields(),
			MustNotContain: tagtest.ID3Secrets(),
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "lower-case ID3 chunk",
			Data:           formFile("AIFF", commChunk, ssndChunk, chunkOf("id3 ", tagtest.ID3v24())),
			WantFields:     []string{"ID3v2 tag", "Artist"},
			MustNotContain: [][]byte{[]byte("ID3"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "ID3 chunk that holds no tag",
			Data:           formFile("AIFF", commChunk, ssndChunk, chunkOf("ID3 ", []byte("Test Not A Tag"))),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("Test Not A Tag")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "unknown chunk",
			Data:           formFile("AIFF", commChunk, chunkOf("ZZZZ", []byte("Test Unknown")), ssndChunk),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("ZZZZ"), []byte("Test Unknown")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "trailing junk",
			Data:           concat(formFile("AIFF", commChunk, ssndChunk), []byte("Test Trailing Junk")),
			WantFields:     []string{"Trailing data"},
			MustNotContain: [][]byte{[]byte("Test Trailing Junk")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "trailing tags",
			Data:           concat(formFile("AIFF", commChunk, ssndChunk), tagtest.APE(), tagtest.ID3v1()),
			WantFields:     []string{"APE tag", "ID3v1 tag"},
			MustNotContain: [][]byte{[]byte("APETAGEX"), []byte("TAG"), []byte(tagtest.Fake)},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "hidden data in the sound offset",
			Data:           formFile("AIFF", commChunk, chunkOf(idSSND, concat([]byte{0, 0, 0, 16, 0, 0, 0, 0}, []byte("Test Hidden Here"), pcm))),
			WantFields:     []string{"Data hidden in sound data offset"},
			MustNotContain: [][]byte{[]byte("Test Hidden Here")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:        "empty sound offset",
			Data:        formFile("AIFF", commChunk, chunkOf(idSSND, concat([]byte{0, 0, 0, 16, 0, 0, 0, 0}, make([]byte, 16), pcm))),
			WantFields:  []string{"Sound data offset padding"},
			MustContain: [][]byte{pcm},
		},
		{
			Name:        "no sound and no frames",
			Data:        formFile("AIFF", chunkOf(idCOMM, comm(0))),
			MustContain: [][]byte{comm(0)},
		},
	}
	coretest.Contract(t, handler{}, cases)
}

func TestRobustness(t *testing.T) {
	coretest.Robustness(t, handler{}, concat(formFile("AIFC",
		chunkOf(idFVER, []byte{0xA2, 0x80, 0x51, 0x40}),
		chunkOf(idCOMM, commC(100)),
		chunkOf("NAME", []byte("Test Title")),
		chunkOf("COMT", comt("Test Comment")),
		chunkOf("MARK", mark("Test Marker")),
		chunkOf("ID3 ", tagtest.ID3v24()),
		chunkOf(idSSND, concat([]byte{0, 0, 0, 8, 0, 0, 0, 0}, make([]byte, 8), sound(200))),
	), tagtest.ID3v1()))
}

func FuzzClean(f *testing.F) {
	commChunk := chunkOf(idCOMM, comm(32))
	ssndChunk := chunkOf(idSSND, ssnd(sound(64)))
	f.Add(formFile("AIFF", commChunk, ssndChunk))
	f.Add(formFile("AIFC", chunkOf(idFVER, []byte{0xA2, 0x80, 0x51, 0x40}), chunkOf(idCOMM, commC(32)), ssndChunk))
	f.Add(formFile("AIFF", chunkOf("NAME", []byte("Test")), chunkOf("AUTH", []byte("Test")), commChunk, chunkOf("COMT", comt("Test")), ssndChunk))
	f.Add(formFile("AIFF", commChunk, chunkOf("MARK", mark("Test", "Marker")), ssndChunk, chunkOf("ID3 ", tagtest.ID3v23())))
	f.Add(concat(formFile("AIFF", commChunk, chunkOf(idSSND, concat([]byte{0, 0, 0, 4, 0, 0, 0, 0}, []byte("Test"), sound(8)))), tagtest.ID3v1()))
	f.Add(formFile("AIFF", chunkOf(idCOMM, comm(0))))
	f.Fuzz(func(t *testing.T, data []byte) { coretest.FuzzTarget(t, handler{}, data) })
}

// The output is exactly the kept chunks, in their order, with the samples untouched.
func TestOutputLayout(t *testing.T) {
	pcm := sound(1000)
	fver := chunkOf(idFVER, []byte{0xA2, 0x80, 0x51, 0x40})
	commChunk := chunkOf(idCOMM, commC(500))
	ssndChunk := chunkOf(idSSND, ssnd(pcm))
	dirty := formFile("AIFC", chunkOf("NAME", []byte("Test Title")), ssndChunk, chunkOf("ANNO", []byte("Test")), commChunk, fver)
	out, _ := clean(t, dirty)
	if want := formFile("AIFC", ssndChunk, chunkOf(idCOMM, commEmptyName(500)), fver); !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

// Sound data offset padding goes, and the offset, block size and chunk size must all
// follow.
func TestSoundOffsetIsNormalized(t *testing.T) {
	pcm := sound(100)
	blockSize := []byte{0, 0, 0x10, 0}
	body := concat([]byte{0, 0, 0, 16}, blockSize, []byte("Test Hidden Here"), pcm)
	out, _ := clean(t, formFile("AIFF", chunkOf(idCOMM, comm(50)), chunkOf(idSSND, body)))
	want := formFile("AIFF", chunkOf(idCOMM, comm(50)), chunkOf(idSSND, ssnd(pcm)))
	if !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

// A pad byte's value is unchecked and can hold anything, so it is never copied.
func TestPadBytesAreZeroed(t *testing.T) {
	pcm := sound(101)
	odd := binary.BigEndian.AppendUint32([]byte(idSSND), uint32(len(pcm)+ssndHeaderLen))
	odd = append(odd, ssnd(pcm)...)
	odd = append(odd, 'S')
	out, _ := clean(t, formFile("AIFF", chunkOf(idCOMM, comm(50)), odd, chunkOf("NAME", []byte("Test"))))
	if want := formFile("AIFF", chunkOf(idCOMM, comm(50)), chunkOf(idSSND, ssnd(pcm))); !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

// Many writers omit the pad on a final odd chunk. The output must be correct whether
// the input had it or not, and its FORM size must count the pad it adds.
func TestMissingFinalPad(t *testing.T) {
	pcm := sound(101)
	odd := binary.BigEndian.AppendUint32([]byte(idSSND), uint32(len(pcm)+ssndHeaderLen))
	odd = append(odd, ssnd(pcm)...)
	body := concat([]byte("AIFF"), chunkOf(idCOMM, comm(50)), odd)
	dirty := concat(binary.BigEndian.AppendUint32([]byte("FORM"), uint32(len(body))), body)
	out, _ := clean(t, dirty)
	if want := formFile("AIFF", chunkOf(idCOMM, comm(50)), chunkOf(idSSND, ssnd(pcm))); !bytes.Equal(out, want) {
		t.Errorf("got %d bytes, want %d", len(out), len(want))
	}
}

func TestReportedValues(t *testing.T) {
	data := formFile("AIFF",
		chunkOf(idCOMM, comm(5)),
		chunkOf("NAME", []byte("Test Title")),
		chunkOf("COMT", comt("Test Comment")),
		chunkOf("MARK", mark("Test Marker", "Test Odd")),
		chunkOf("APPL", []byte("TESTdata")),
		chunkOf(idSSND, ssnd(sound(10))),
	)
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		field string
		want  []string
	}{
		{"Title", []string{"Test Title"}},
		{"Comment", []string{"Test Comment"}},
		{"Marker", []string{"Test Marker", "Test Odd"}},
		{"Application data", []string{"TEST, 8 bytes"}},
	} {
		got := values(rep, c.field)
		if len(got) != len(c.want) {
			t.Errorf("field %q has values %q, want %q", c.field, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("field %q has values %q, want %q", c.field, got, c.want)
			}
		}
	}
}

// Damage inside a chunk being removed costs detail in the report, never the file.
func TestDamagedDroppedChunksAreStillRemoved(t *testing.T) {
	for _, c := range []struct {
		name string
		id   string
		body []byte
	}{
		{"COMT too short", "COMT", []byte{0}},
		{"COMT count too large", "COMT", []byte{0xFF, 0xFF}},
		{"COMT length too large", "COMT", []byte{0, 1, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF, 'T', 'e', 's', 't'}},
		{"MARK too short", "MARK", []byte{0}},
		{"MARK count too large", "MARK", []byte{0xFF, 0xFF}},
		{"MARK length too large", "MARK", []byte{0, 1, 0, 1, 0, 0, 0, 0, 0xFF, 'T', 'e', 's', 't'}},
		{"short APPL", "APPL", []byte("TE")},
	} {
		t.Run(c.name, func(t *testing.T) {
			pcm := sound(10)
			out, _ := clean(t, formFile("AIFF", chunkOf(idCOMM, comm(5)), chunkOf(c.id, c.body), chunkOf(idSSND, ssnd(pcm))))
			if want := formFile("AIFF", chunkOf(idCOMM, comm(5)), chunkOf(idSSND, ssnd(pcm))); !bytes.Equal(out, want) {
				t.Errorf("got %d bytes, want %d", len(out), len(want))
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	commChunk := chunkOf(idCOMM, comm(5))
	ssndChunk := chunkOf(idSSND, ssnd(sound(10)))
	good := formFile("AIFF", commChunk, ssndChunk)
	overrun := bytes.Clone(good)
	binary.BigEndian.PutUint32(overrun[4:], uint32(len(good)))
	tiny := bytes.Clone(good)
	binary.BigEndian.PutUint32(tiny[4:], 3)

	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"other FORM", bytes.Replace(good, []byte("AIFF"), []byte("8SVX"), 1), core.ErrUnsupported},
		{"not IFF", []byte("Test not an AIFF file at all"), core.ErrUnsupported},

		{"no COMM", formFile("AIFF", ssndChunk), core.ErrMalformed},
		{"no SSND with frames declared", formFile("AIFF", commChunk), core.ErrMalformed},
		{"two COMM chunks", formFile("AIFF", commChunk, commChunk, ssndChunk), core.ErrMalformed},
		{"two SSND chunks", formFile("AIFF", commChunk, ssndChunk, ssndChunk), core.ErrMalformed},
		{"two FVER chunks", formFile("AIFC", chunkOf(idFVER, make([]byte, 4)), chunkOf(idFVER, make([]byte, 4)), chunkOf(idCOMM, commC(5)), ssndChunk), core.ErrMalformed},
		{"FVER of the wrong size", formFile("AIFC", chunkOf(idFVER, []byte("Test Hidden")), chunkOf(idCOMM, commC(5)), ssndChunk), core.ErrMalformed},
		{"short COMM", formFile("AIFF", chunkOf(idCOMM, make([]byte, 17)), ssndChunk), core.ErrMalformed},
		{"AIFF-C COMM without compression", formFile("AIFC", chunkOf(idCOMM, comm(5)), ssndChunk), core.ErrMalformed},
		{"short SSND", formFile("AIFF", commChunk, chunkOf(idSSND, make([]byte, 7))), core.ErrMalformed},
		{"SSND offset past its end", formFile("AIFF", commChunk, chunkOf(idSSND, []byte{0, 0, 1, 0, 0, 0, 0, 0})), core.ErrMalformed},
		{"chunk past the FORM", formFile("AIFF", commChunk, concat([]byte(idSSND), []byte{0, 0, 0xFF, 0xFF})), core.ErrMalformed},
		{"stray bytes in the FORM", formFile("AIFF", commChunk, ssndChunk, []byte("xx")), core.ErrMalformed},
		{"FORM past the end of the file", overrun, core.ErrMalformed},
		{"FORM too small", tiny, core.ErrMalformed},
		{"too short", []byte("FORM"), core.ErrMalformed},
		{"empty", nil, core.ErrMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := (handler{}).Clean(context.Background(), core.BytesSource(c.data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Clean = %v, want %v", err, c.want)
			}
			if _, err := (handler{}).Inspect(context.Background(), core.BytesSource(c.data), core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Inspect = %v, want %v", err, c.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	good := formFile("AIFF", chunkOf(idCOMM, comm(5)), chunkOf(idSSND, ssnd(sound(10))))
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"AIFF", good, true},
		{"AIFF-C", bytes.Replace(good, []byte("AIFF"), []byte("AIFC"), 1), true},
		{"other FORM", bytes.Replace(good, []byte("AIFF"), []byte("8SVX"), 1), false},
		{"other", []byte("Test not an AIFF file at all"), false},
		{"short", []byte("FORM"), false},
		{"empty", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := (handler{}).Match(c.data[:min(len(c.data), core.HeadSize)], core.BytesSource(c.data)); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := formFile("AIFF", chunkOf(idCOMM, comm(5)), chunkOf("NAME", []byte("Test")), chunkOf(idSSND, ssnd(sound(10))))
	if _, err := (handler{}).Clean(ctx, core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Clean = %v", err)
	}
}

func TestStreamsWithoutLoadingTheFile(t *testing.T) {
	const soundLen = tagtest.StreamingFileSize
	head := []byte("FORM\x00\x00\x00\x00AIFF")
	commChunk := chunkOf(idCOMM, comm(soundLen/2))
	name := chunkOf("NAME", []byte("Test Title"))
	ssndHeader := binary.BigEndian.AppendUint32([]byte(idSSND), soundLen+ssndHeaderLen)
	ssndHeader = append(ssndHeader, make([]byte, ssndHeaderLen)...)
	prefix := concat(head, commChunk, name, ssndHeader)
	binary.BigEndian.PutUint32(prefix[4:], uint32(len(prefix)-8+soundLen))

	src := tagtest.Repeating{Prefix: prefix, Period: sound(4096), Total: int64(len(prefix)) + soundLen}
	var out tagtest.CountingWriter
	allocated := tagtest.Allocated(func() {
		if _, err := (handler{}).Clean(context.Background(), src, &out, core.Options{}); err != nil {
			t.Error(err)
		}
	})
	want := int64(len(head)+len(commChunk)+len(ssndHeader)) + soundLen
	if out.N != want {
		t.Errorf("wrote %d bytes, want %d", out.N, want)
	}
	if allocated > tagtest.StreamingBudget {
		t.Errorf("allocated %d bytes to clean a %d-byte file", allocated, src.Total)
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != formatID || f.Family != core.FamilyAudio || len(f.Extensions) == 0 || len(f.Limitations) == 0 {
		t.Errorf("incomplete format: %+v", f)
	}
}

// commEmptyName is what cleaning leaves of an AIFF-C COMM: the fixed fields, the
// compression type, and a compression name of nothing.
func commEmptyName(frames uint32) []byte {
	return append(comm(frames), "NONE\x00\x00"...)
}

// The compression name is free text that no decoder reads, and bytes past it or past
// the fixed fields of COMM are read by nothing at all.
func TestCOMMKeepsOnlyWhatDecodersRead(t *testing.T) {
	pcm := sound(1000)
	ssndChunk := chunkOf(idSSND, ssnd(pcm))
	secret := []byte("Test Hidden Name")
	named := func(name []byte) []byte {
		out := append(comm(500), "NONE"...)
		out = append(out, byte(len(name)))
		out = append(out, name...)
		if len(name)%2 == 0 {
			out = append(out, 0)
		}
		return out
	}
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name:           "past the end of an AIFF COMM",
			Data:           formFile("AIFF", chunkOf(idCOMM, concat(comm(500), secret)), ssndChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunkOf(idCOMM, comm(500)), pcm},
		},
		{
			Name:           "as the compression name",
			Data:           formFile("AIFC", chunkOf(idCOMM, named(secret)), ssndChunk),
			WantFields:     []string{"Compression name"},
			MustNotContain: [][]byte{secret},
			MustContain:    [][]byte{chunkOf(idCOMM, commEmptyName(500)), pcm},
		},
		{
			Name:           "past the compression name",
			Data:           formFile("AIFC", chunkOf(idCOMM, concat(commC(500), secret)), ssndChunk),
			WantFields:     []string{"Compression name", "Data hidden in padding"},
			MustNotContain: [][]byte{secret, []byte("not compressed")},
			MustContain:    [][]byte{chunkOf(idCOMM, commEmptyName(500)), pcm},
		},
		{
			Name:        "an AIFF-C COMM with no name at all",
			Data:        formFile("AIFC", chunkOf(idCOMM, append(comm(500), "NONE"...)), ssndChunk),
			MustContain: [][]byte{chunkOf(idCOMM, commEmptyName(500)), pcm},
		},
		{
			Name:           "as the FVER timestamp",
			Data:           formFile("AIFC", chunkOf(idFVER, []byte("TEST")), chunkOf(idCOMM, commEmptyName(500)), ssndChunk),
			WantFields:     []string{"Data hidden in padding"},
			MustNotContain: [][]byte{[]byte("TEST")},
			MustContain:    [][]byte{chunkOf(idFVER, []byte{0xA2, 0x80, 0x51, 0x40}), pcm},
		},
		{
			Name:           "as the sound block size",
			Data:           formFile("AIFF", chunkOf(idCOMM, comm(500)), chunkOf(idSSND, concat([]byte{0, 0, 0, 0}, []byte("TEST"), pcm))),
			MustNotContain: [][]byte{[]byte("TEST")},
			MustContain:    [][]byte{chunkOf(idSSND, ssnd(pcm))},
		},
	})
}

// Without CHAN, a surround file's channels are left for the player to guess at.
func TestChannelLayoutIsKept(t *testing.T) {
	pcm := sound(1000)
	layout := []byte{0x00, 0x79, 0x00, 0x06, 0, 0, 0, 0, 0, 0, 0, 0}
	withDescriptions := concat([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, []byte("Test Description Bytes"))
	commChunk := chunkOf(idCOMM, comm(500))
	ssndChunk := chunkOf(idSSND, ssnd(pcm))
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name:        "a plain layout",
			Data:        formFile("AIFF", commChunk, chunkOf(idCHAN, layout), ssndChunk),
			MustContain: [][]byte{chunkOf(idCHAN, layout), pcm},
		},
		{
			Name:           "a layout with free-form descriptions",
			Data:           formFile("AIFF", commChunk, chunkOf(idCHAN, withDescriptions), ssndChunk),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("Test Description")},
			MustContain:    [][]byte{pcm},
		},
		{
			Name:           "a second layout",
			Data:           formFile("AIFF", commChunk, chunkOf(idCHAN, layout), chunkOf(idCHAN, []byte("Test Second!")), ssndChunk),
			WantFields:     []string{"Unknown chunk"},
			MustNotContain: [][]byte{[]byte("Test Second")},
			MustContain:    [][]byte{chunkOf(idCHAN, layout), pcm},
		},
	})
}

// Sound bytes past the declared frames stay because ffmpeg plays them, which is known
// the moment the sizes are compared, so the user has to be told.
func TestSoundPastTheDeclaredFramesWarns(t *testing.T) {
	pcm := sound(1000)
	for _, c := range []struct {
		name string
		comm []byte
		form string
		warn bool
	}{
		{"exact", comm(500), "AIFF", false},
		{"sound past the frames", comm(400), "AIFF", true},
		{"no frames declared", comm(0), "AIFF", true},
		{"AIFF-C uncompressed", append(comm(400), "NONE\x00\x00"...), "AIFC", true},
		{"AIFF-C uncompressed, exact", append(comm(500), "NONE\x00\x00"...), "AIFC", false},
		{"AIFF-C little-endian", append(comm(400), "sowt\x00\x00"...), "AIFC", true},
		{"compressed, so the size per frame is unknown", append(comm(400), "ima4\x00\x00"...), "AIFC", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, rep := clean(t, formFile(c.form, chunkOf(idCOMM, c.comm), chunkOf(idSSND, ssnd(pcm))))
			if !bytes.Contains(out, pcm) {
				t.Error("sound was lost")
			}
			if got := len(rep.Warnings) > 0; got != c.warn {
				t.Errorf("warnings = %q, want a warning: %v", rep.Warnings, c.warn)
			}
		})
	}

	t.Run("one spare byte, as a counted pad", func(t *testing.T) {
		_, rep := clean(t, formFile("AIFF", chunkOf(idCOMM, comm(500)), chunkOf(idSSND, ssnd(sound(1001)))))
		if len(rep.Warnings) > 0 {
			t.Errorf("warnings = %q", rep.Warnings)
		}
	})
}

// A writer that never went back to fill in its sizes leaves an empty sound chunk with
// the samples after it, and dropping them as trailing data would leave nothing.
func TestEmptySoundWithBytesAfterItIsRefused(t *testing.T) {
	commChunk := chunkOf(idCOMM, comm(500))
	empty := chunkOf(idSSND, ssnd(nil))
	data := concat(formFile("AIFF", commChunk, empty), sound(1000))
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("Clean = %v, want %v", err, core.ErrMalformed)
	}
	// A tag there is accounted for, and a sound chunk with sound in it knows its size.
	clean(t, concat(formFile("AIFF", commChunk, empty), tagtest.ID3v1()))
	clean(t, concat(formFile("AIFF", commChunk, chunkOf(idSSND, ssnd(sound(1000)))), sound(1000)))
}

// A comment or marker chunk that decodes to nothing is still a chunk being removed,
// with bytes in it, and the report must not say the file held nothing.
func TestUndecodableTextChunksAreReported(t *testing.T) {
	commChunk := chunkOf(idCOMM, comm(5))
	ssndChunk := chunkOf(idSSND, ssnd(sound(10)))
	for name, c := range map[string]struct {
		id, field string
		body      []byte
	}{
		"comments that do not fit their count": {"COMT", "Annotation", concat([]byte{0, 5}, []byte("Test!!"))},
		"a comment count of zero with data":    {"COMT", "Annotation", concat([]byte{0, 0}, []byte("Test Hidden"))},
		"markers that do not fit their count":  {"MARK", "Marker", concat([]byte{0, 5}, []byte("Test!!"))},
		"a marker count of zero with data":     {"MARK", "Marker", concat([]byte{0, 0}, []byte("Test Hidden"))},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, formFile("AIFF", commChunk, chunkOf(c.id, c.body), ssndChunk))
			if bytes.Contains(out, []byte("Test")) {
				t.Error("the chunk was kept")
			}
			if len(values(rep, c.field)) == 0 {
				t.Errorf("nothing was reported: %+v", rep.Findings)
			}
		})
	}
}
