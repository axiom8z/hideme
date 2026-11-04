package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

func TestDataAroundTheArchiveIsDroppedAndReported(t *testing.T) {
	base := docx(t)
	const before = "Test Prefix Secret, not a ZIP"
	const after = "Test Trailer Secret"

	// Prepending data leaves the offsets wrong, which archive/zip compensates for.
	withBefore := append([]byte(before), base...)
	withAfter := append(bytes.Clone(base), after...)

	for name, tc := range map[string]struct {
		data   []byte
		field  string
		secret string
		size   int
	}{
		"before": {withBefore, "Data before the ZIP archive", before, len(before)},
		"after":  {withAfter, "Data after the ZIP archive", after, len(after)},
	} {
		t.Run(name, func(t *testing.T) {
			if !ooxml.Match(tc.data, core.BytesSource(tc.data)) {
				t.Fatal("Match is false")
			}
			out, rep := clean(t, ooxml, tc.data)
			if bytes.Contains(out, []byte(tc.secret)) {
				t.Errorf("output still contains %q", tc.secret)
			}
			if !hasValue(rep, tc.field, fmt.Sprintf("%d bytes", tc.size)) {
				t.Errorf("%s = %q, want %d bytes", tc.field, values(rep, tc.field), tc.size)
			}
			again, err := ooxml.Inspect(context.Background(), core.BytesSource(out), core.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(values(again, tc.field)) != 0 {
				t.Errorf("the output still reports %s", tc.field)
			}
		})
	}
}

func TestEntryMetadataIsDroppedAndReportedOnce(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.SetComment("Test Archive Comment"); err != nil {
		t.Fatal(err)
	}
	for _, p := range docxParts(docxMain) {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:    p.name,
			Method:  zip.Deflate,
			Comment: "Test Entry Comment",
			Extra:   []byte{0x99, 0x99, 4, 0, 'T', 'E', 'S', 'T'},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out, rep := clean(t, ooxml, buf.Bytes())
	for _, secret := range []string{"Test Archive Comment", "Test Entry Comment"} {
		if bytes.Contains(out, []byte(secret)) {
			t.Errorf("output still contains %q", secret)
		}
	}
	if !hasValue(rep, "ZIP comment", "Test Archive Comment") {
		t.Errorf("archive comment not reported: %+v", rep.Findings)
	}
	if got := values(rep, "ZIP entry comment"); len(got) != 1 {
		t.Errorf("the same entry comment should be reported once, got %q", got)
	}
	if got := values(rep, "ZIP extra fields"); len(got) != 1 {
		t.Errorf("extra fields should be reported once, got %q", got)
	}
}

func TestEntryTimesAreReportedOnce(t *testing.T) {
	_, rep := clean(t, ooxml, docx(t))
	got := values(rep, "ZIP entry times")
	if len(got) != 1 || !strings.Contains(got[0], "2001-02-03 04:05:06") {
		t.Errorf("ZIP entry times = %q", got)
	}
}

func TestRejectedArchives(t *testing.T) {
	base := docxParts(docxMain)
	dup := zipOf(t, append(base, part{"word/document.xml", "<again/>"})...)
	caseDup := zipOf(t, append(base, part{"WORD/DOCUMENT.XML", "<again/>"})...)
	escaped := zipOf(t, append(base, part{"word/%64ocument.xml", "<again/>"})...)
	backslash := zipOf(t, append(base, part{`docProps\core.xml`, coreXML})...)
	dots := zipOf(t, append(base, part{"word/../secret.xml", "<a/>"})...)
	absolute := zipOf(t, append(base, part{"/secret.xml", "<a/>"})...)

	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"duplicate name":       {dup, core.ErrMalformed},
		"names differ by case": {caseDup, core.ErrMalformed},
		"names differ by %":    {escaped, core.ErrMalformed},
		"backslash in a name":  {backslash, core.ErrMalformed},
		"dot segment":          {dots, core.ErrMalformed},
		"absolute name":        {absolute, core.ErrMalformed},
		"encrypted entry":      {setEntryFlag(t, docx(t), 0x1), core.ErrEncrypted},
		"unknown compression":  {setMethod(t, docx(t), 99), core.ErrUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := ooxml.Clean(context.Background(), core.BytesSource(tc.data), &out, core.Options{})
			if !errors.Is(err, tc.want) {
				t.Errorf("Clean: got %v, want %v", err, tc.want)
			}
			if _, err := ooxml.Inspect(context.Background(), core.BytesSource(tc.data), core.Options{}); !errors.Is(err, tc.want) {
				t.Errorf("Inspect: got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestEncryptedEntryIsAlsoMalformed: neither format uses ZIP encryption, so such an
// entry is fairly described by both sentinels.
func TestEncryptedEntryIsAlsoMalformed(t *testing.T) {
	data := setEntryFlag(t, docx(t), 0x1)
	_, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	if !errors.Is(err, core.ErrEncrypted) || !errors.Is(err, core.ErrMalformed) {
		t.Errorf("got %v, want both ErrEncrypted and ErrMalformed", err)
	}
}

// centralEntries calls fn with the offset of every central directory record.
func centralEntries(t *testing.T, data []byte, fn func(off int)) {
	t.Helper()
	end := bytes.LastIndex(data, []byte("PK\x05\x06"))
	if end < 0 {
		t.Fatal("no end of central directory")
	}
	off := int(binary.LittleEndian.Uint32(data[end+16:]))
	for bytes.HasPrefix(data[off:], []byte("PK\x01\x02")) {
		fn(off)
		off += 46 + int(binary.LittleEndian.Uint16(data[off+28:])) + int(binary.LittleEndian.Uint16(data[off+30:])) + int(binary.LittleEndian.Uint16(data[off+32:]))
	}
}

func setEntryFlag(t *testing.T, data []byte, flag uint16) []byte {
	t.Helper()
	data = bytes.Clone(data)
	centralEntries(t, data, func(off int) {
		binary.LittleEndian.PutUint16(data[off+8:], binary.LittleEndian.Uint16(data[off+8:])|flag)
	})
	return data
}

func setMethod(t *testing.T, data []byte, method uint16) []byte {
	t.Helper()
	data = bytes.Clone(data)
	centralEntries(t, data, func(off int) {
		binary.LittleEndian.PutUint16(data[off+10:], method)
	})
	return data
}

func TestTooManyEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := range maxEntries + 1 {
		if _, err := zw.CreateRaw(&zip.FileHeader{Name: fmt.Sprintf("p%d", i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	_, _, err := openArchive(context.Background(), core.BytesSource(data), defaultLimits)
	if !errors.Is(err, core.ErrMalformed) {
		t.Errorf("got %v, want ErrMalformed", err)
	}
}

// bomb returns a document with an entry that expands to n zero bytes.
func bomb(t *testing.T, n int) []byte {
	t.Helper()
	return zipOf(t, append(docxParts(docxMain), part{"word/media/bomb.bin", strings.Repeat("\x00", n)})...)
}

func TestExpansionLimits(t *testing.T) {
	const mib = 1 << 20
	small := ooxmlHandler{limits: limits{maxEntry: mib, maxTotal: 64 * mib}}
	if _, err := small.Inspect(context.Background(), core.BytesSource(bomb(t, 3*mib)), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("entry limit: got %v, want ErrTooLarge", err)
	}
	total := ooxmlHandler{limits: limits{maxEntry: 64 * mib, maxTotal: 2 * mib}}
	if _, err := total.Inspect(context.Background(), core.BytesSource(bomb(t, 3*mib)), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("total limit: got %v, want ErrTooLarge", err)
	}
	if _, err := small.Inspect(context.Background(), core.BytesSource(docx(t)), core.Options{}); err != nil {
		t.Errorf("an ordinary document hits the limits: %v", err)
	}
}

// TestDeclaredSizeIsNotBelieved: a bomb declaring a tiny size must not slip through
// on the strength of its own say-so, and one declaring an enormous size must not
// cause an enormous allocation.
func TestDeclaredSizeIsNotBelieved(t *testing.T) {
	for _, declared := range []uint32{1, 0xFFFFFFF0} {
		data := bytes.Clone(zipOf(t, append(docxParts(docxMain), part{"word/media/bomb.bin", strings.Repeat("\x00", 1<<20)})...))
		target := []byte("word/media/bomb.bin")
		centralEntries(t, data, func(off int) {
			nameLen := int(binary.LittleEndian.Uint16(data[off+28:]))
			if bytes.Equal(data[off+46:off+46+nameLen], target) {
				binary.LittleEndian.PutUint32(data[off+24:], declared)
			}
		})
		_, err := ooxml.Inspect(context.Background(), core.BytesSource(data), core.Options{})
		if !errors.Is(err, core.ErrMalformed) {
			t.Errorf("declared %d: got %v, want ErrMalformed", declared, err)
		}
	}
}

// TestBudgetSpansNestedDocuments: each level here is under the limits alone, so
// only a budget shared across the recursion can stop the expansion as a whole.
func TestBudgetSpansNestedDocuments(t *testing.T) {
	inner := bomb(t, 1<<20)
	parts := docxParts(docxMain)
	for i := range 8 {
		parts = append(parts, part{fmt.Sprintf("word/embeddings/d%d.docx", i), string(inner)})
	}
	data := zipOf(t, parts...)
	h := ooxmlHandler{limits: limits{maxEntry: 16 << 20, maxTotal: 4 << 20}}
	if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestCancelledContextStopsWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, h := range []core.Handler{ooxml, odfHandler{limits: defaultLimits}} {
		data := docx(t)
		if h.Format().ID == "odf" {
			data = odt(t)
		}
		if _, err := h.Inspect(ctx, core.BytesSource(data), core.Options{}); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: got %v, want context.Canceled", h.Format().ID, err)
		}
	}
}

func TestValidEntryName(t *testing.T) {
	for name, want := range map[string]bool{
		"word/document.xml":   true,
		"[Content_Types].xml": true,
		"Object 1/meta.xml":   true,
		"Thumbnails/":         true,
		"word/ünï.xml":        true,
		"":                    false,
		"/abs.xml":            false,
		`a\b.xml`:             false,
		"a/../b.xml":          false,
		"./a.xml":             false,
		"a//b.xml":            false,
		"a\x00b.xml":          false,
		"a\nb.xml":            false,
	} {
		if got := validEntryName(name); got != want {
			t.Errorf("validEntryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	for _, tc := range []struct{ source, target, want string }{
		{"", "word/document.xml", "word/document.xml"},
		{"", "/word/document.xml", "word/document.xml"},
		{"word/document.xml", "media/a.jpeg", "word/media/a.jpeg"},
		{"word/document.xml", "../customXml/item1.xml", "customxml/item1.xml"},
		{"word/document.xml", "/docProps/core.xml", "docprops/core.xml"},
		{"word/document.xml", `..\docProps\core.xml`, "docprops/core.xml"},
		{"word/document.xml", "my%20image.png", "word/my image.png"},
		{"word/document.xml", "slides/s.xml#frag", "word/slides/s.xml"},
		{"word/document.xml", "../../../etc/passwd", "etc/passwd"},
		{"word/document.xml", "", ""},
	} {
		if got := resolveTarget(tc.source, tc.target); got != tc.want {
			t.Errorf("resolveTarget(%q, %q) = %q, want %q", tc.source, tc.target, got, tc.want)
		}
	}
}

func TestLooksLikeFilePath(t *testing.T) {
	for _, tc := range []struct {
		target string
		bare   bool
		want   bool
	}{
		{"file:///C:/Users/x/a.xlsx", false, true},
		{`\\server\share\a.xlsx`, false, true},
		{"//server/share/a.xlsx", false, true},
		{`C:\Users\x\a.xlsx`, false, true},
		{"C:/Users/x/a.xlsx", false, true},
		{"A: a sunset", false, false},
		{"../a.png", false, true},
		{"/Users/x/a.png", false, true},
		{"https://example.invalid/a", true, false},
		{"mailto:x@example.invalid", true, false},
		{"#Sheet1!A1", true, false},
		{"budget.xlsx", true, true},
		{"budget.xlsx", false, false},
		{"Pictures/a.png", false, false},
		{"", true, false},
	} {
		if got := looksLikeFilePath(tc.target, tc.bare); got != tc.want {
			t.Errorf("looksLikeFilePath(%q, %v) = %v, want %v", tc.target, tc.bare, got, tc.want)
		}
	}
}
