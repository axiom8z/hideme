package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSafeText(t *testing.T) {
	long := strings.Repeat("a", maxSafeText)
	wide := strings.Repeat("é", maxSafeText)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is unchanged", "Test Author", "Test Author"},
		{"empty", "", ""},
		{"non-ASCII is kept", "Müller 東京", "Müller 東京"},
		{"terminal colour escape loses its ESC", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"terminal title escape loses its controls", "\x1b]0;owned\x07text", "]0;ownedtext"},
		{"C1 control introducer", "a\u009b31mb", "a31mb"},
		{"backspace cannot overwrite earlier output", "safe\x08\x08\x08\x08evil", "safeevil"},
		{"NUL", "a\x00b", "ab"},
		{"bidi override", "good\u202Etxt.exe", "goodtxt.exe"},
		{"bidi isolate", "a\u2066b\u2069c", "abc"},
		{"zero-width joiner", "a\u200Db", "ab"},
		{"byte order mark", "\uFEFFtext", "text"},
		{"invalid UTF-8 is dropped", "a\xffb\xc0c", "abc"},
		{"truncated UTF-8 sequence is dropped", "abc\xe6\x9d", "abc"},
		{"a real replacement character is kept", "a\uFFFDb", "a\uFFFDb"},
		{"newline cannot forge a report line", "x\nGPS: none", "x GPS: none"},
		{"carriage return and tab", "a\rb\tc", "a b c"},
		{"unicode line separators cannot forge a report line", "a\u2028b\u2029c", "a b c"},
		{"next line control is removed", "a\u0085b", "ab"},
		{"surrounding space is trimmed", "  text \n", "text"},
		{"only controls gives empty", "\x1b\x00\u202E", ""},
		{"exactly at the limit is not truncated", long, long},
		{"one over the limit is truncated", long + "b", long + "..."},
		{"the limit counts characters, not bytes", wide + "é", wide + "..."},
		{"removed characters do not count toward the limit", strings.Repeat("\x1b", 500) + "ok", "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeText(tt.in); got != tt.want {
				t.Errorf("SafeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func FuzzSafeText(f *testing.F) {
	f.Add("\x1b[31m\u202Eabc\xff\n")
	f.Fuzz(func(t *testing.T, s string) {
		got := SafeText(s)
		if !utf8.ValidString(got) {
			t.Errorf("SafeText(%q) = %q is not valid UTF-8", s, got)
		}
		if strings.ContainsFunc(got, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.In(r, unicode.Zl, unicode.Zp)
		}) {
			t.Errorf("SafeText(%q) = %q contains an unsafe character", s, got)
		}
		if n := utf8.RuneCountInString(got); n > maxSafeText+len("...") {
			t.Errorf("SafeText(%q) is %d characters long", s, n)
		}
	})
}

func TestReportAddAndWarnSanitise(t *testing.T) {
	var r Report
	r.Add(Finding{Field: "F\x1b", Value: "V\u202E", Where: "W\n", Reason: "R\x00"})
	r.Warn("bad %s", "\x1b[2Jvalue")
	got := r.Findings[0]
	if got.Field != "F" || got.Value != "V" || got.Where != "W" || got.Reason != "R" {
		t.Errorf("Add did not sanitise every string: %+v", got)
	}
	if r.Warnings[0] != "bad [2Jvalue" {
		t.Errorf("Warn did not sanitise: %q", r.Warnings[0])
	}
}

func TestReportResidual(t *testing.T) {
	var nilReport *Report
	if got := nilReport.Residual(); got != nil {
		t.Errorf("Residual on a nil report = %v, want nil", got)
	}
	if got := (&Report{}).Residual(); got != nil {
		t.Errorf("Residual on an empty report = %v, want nil", got)
	}
	r := &Report{Findings: []Finding{
		{Field: "kept", Action: ActionRetain},
		{Field: "removed", Action: ActionRemove},
		{Field: "anonymized", Action: ActionAnonymize},
		{Field: "also kept", Action: ActionRetain},
	}}
	got := r.Residual()
	if len(got) != 2 || got[0].Field != "removed" || got[1].Field != "anonymized" {
		t.Errorf("Residual = %+v, want the removed and anonymized findings", got)
	}
}

// A TIFF inside a document can lose frames just as a TIFF on its own can, so the flag
// has to climb out of nested reports along with their warnings.
func TestMergeCarriesMayLoseContent(t *testing.T) {
	var r Report
	r.Merge("word/media/scan.tif", &Report{})
	if r.MayLoseContent {
		t.Error("the flag appeared from nowhere")
	}
	r.Merge("word/media/stack.tif", &Report{MayLoseContent: true})
	r.Merge("word/media/other.tif", &Report{})
	if !r.MayLoseContent {
		t.Error("Merge dropped MayLoseContent, so --in-place would replace the original")
	}
}

func TestSafeMessageKeepsLongerExplanationsButStaysBounded(t *testing.T) {
	long := strings.Repeat("word ", 100)
	if got := SafeMessage("a\x1b[2J‮b " + long); strings.ContainsAny(got, "\x1b‮") || strings.Contains(got, "...") {
		t.Errorf("SafeMessage(%q)", got)
	}
	if got := SafeMessage(strings.Repeat("x", 5000)); len(got) > 1000 || !strings.HasSuffix(got, "...") {
		t.Errorf("SafeMessage is not bounded: %d bytes", len(got))
	}
}

// A warning ends in what the user should do, and being nested in a long path must not
// be what cuts that off.
func TestWarningKeepsItsEndWhenNested(t *testing.T) {
	var nested, r Report
	nested.Warn("%s; keep the original", strings.Repeat("word ", 35))
	r.Merge("word/media/a-rather-long-name-for-an-embedded-picture.tif", &nested)
	if !strings.HasSuffix(r.Warnings[0], "keep the original") {
		t.Errorf("warning cut off: %q", r.Warnings[0])
	}
}

func TestReportMerge(t *testing.T) {
	t.Run("a nil report is ignored", func(t *testing.T) {
		var r Report
		r.Merge("image1.png", nil)
		if len(r.Findings) != 0 || len(r.Warnings) != 0 {
			t.Errorf("merging nil changed the report: %+v", r)
		}
	})
	t.Run("locations are prefixed and order is kept", func(t *testing.T) {
		r := Report{Format: "docx"}
		r.Add(Finding{Field: "own"})
		r.Merge("word/media/image1.png", &Report{
			Format: "png",
			Findings: []Finding{
				{Field: "nested", Where: "tEXt", Action: ActionRemove},
				{Field: "no location", Action: ActionRetain, Reason: "why"},
			},
			Warnings: []string{"could not read"},
		})
		want := []Finding{
			{Field: "own"},
			{Field: "nested", Where: "word/media/image1.png/tEXt", Action: ActionRemove},
			{Field: "no location", Where: "word/media/image1.png", Action: ActionRetain, Reason: "why"},
		}
		if len(r.Findings) != len(want) {
			t.Fatalf("got %d findings, want %d: %+v", len(r.Findings), len(want), r.Findings)
		}
		for i := range want {
			if r.Findings[i] != want[i] {
				t.Errorf("finding %d = %+v, want %+v", i, r.Findings[i], want[i])
			}
		}
		if len(r.Warnings) != 1 || r.Warnings[0] != "word/media/image1.png: could not read" {
			t.Errorf("warnings = %q", r.Warnings)
		}
		if r.Format != "docx" {
			t.Errorf("Merge changed the outer format to %q", r.Format)
		}
	})
	t.Run("a hostile location is sanitised", func(t *testing.T) {
		var r Report
		r.Merge("evil\x1b[2J.png", &Report{Findings: []Finding{{Field: "f"}}})
		if got := r.Findings[0].Where; got != "evil[2J.png" {
			t.Errorf("Where = %q", got)
		}
	})
	t.Run("the nested report is not modified", func(t *testing.T) {
		nested := &Report{Findings: []Finding{{Field: "f", Where: "inner"}}}
		var r Report
		r.Merge("outer", nested)
		if nested.Findings[0].Where != "inner" {
			t.Errorf("nested Where became %q", nested.Findings[0].Where)
		}
	})
}

// sequence returns n distinct-looking bytes, so a misplaced copy shows up.
func sequence(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

// failingWriter fails every write, to prove write errors are not swallowed.
type failingWriter struct{}

var errWrite = errors.New("disk full")

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// lyingSource claims more bytes than it can deliver, as a file truncated by
// another process would.
type lyingSource struct{ size int64 }

func (s lyingSource) Size() int64                       { return s.size }
func (s lyingSource) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestCopyRange(t *testing.T) {
	data := sequence(4096)
	tests := []struct {
		name    string
		off, n  int64
		wantErr bool
	}{
		{"whole file", 0, 4096, false},
		{"middle", 100, 200, false},
		{"tail", 4000, 96, false},
		{"empty at the start", 0, 0, false},
		{"empty at the end", 4096, 0, false},
		{"one past the end", 4000, 97, true},
		{"starts past the end", 4097, 0, true},
		{"negative offset", -1, 10, true},
		{"negative length", 0, -1, true},
		{"length that would overflow", 1, 1<<63 - 1, true},
		{"offset that would overflow", 1<<63 - 1, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := CopyRange(context.Background(), &out, BytesSource(data), tt.off, tt.n)
			if tt.wantErr {
				if !errors.Is(err, ErrMalformed) {
					t.Errorf("err = %v, want ErrMalformed", err)
				}
				if out.Len() != 0 {
					t.Errorf("wrote %d bytes before failing", out.Len())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), data[tt.off:tt.off+tt.n]) {
				t.Error("copied bytes differ from the source")
			}
		})
	}
}

func TestCopyRangeLargerThanItsBuffer(t *testing.T) {
	data := sequence(copyBufferSize*2 + 123)
	var out bytes.Buffer
	if err := CopyRange(context.Background(), &out, BytesSource(data), 5, int64(len(data))-5); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data[5:]) {
		t.Error("copied bytes differ from the source")
	}
}

func TestCopyRangeFailures(t *testing.T) {
	data := sequence(16)
	if err := CopyRange(cancelled(), io.Discard, BytesSource(data), 0, 16); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
	if err := CopyRange(context.Background(), failingWriter{}, BytesSource(data), 0, 16); !errors.Is(err, errWrite) {
		t.Errorf("failing writer: err = %v", err)
	}
	err := CopyRange(context.Background(), io.Discard, lyingSource{size: 16}, 0, 16)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short read: err = %v", err)
	}
}

func TestWriteZeros(t *testing.T) {
	for _, n := range []int64{0, 1, 4096, copyBufferSize, copyBufferSize + 1, copyBufferSize*2 + 7} {
		var out bytes.Buffer
		if err := WriteZeros(context.Background(), &out, n); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if int64(out.Len()) != n {
			t.Errorf("n=%d: wrote %d bytes", n, out.Len())
		}
		if bytes.ContainsFunc(out.Bytes(), func(r rune) bool { return r != 0 }) {
			t.Errorf("n=%d: wrote a non-zero byte", n)
		}
	}
	if err := WriteZeros(context.Background(), io.Discard, -1); !errors.Is(err, ErrMalformed) {
		t.Errorf("negative length: err = %v", err)
	}
	if err := WriteZeros(cancelled(), io.Discard, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
	if err := WriteZeros(context.Background(), failingWriter{}, 1); !errors.Is(err, errWrite) {
		t.Errorf("failing writer: err = %v", err)
	}
}

func TestCopyPatched(t *testing.T) {
	src := []byte("0123456789")
	tests := []struct {
		name    string
		patches []Patch
		want    string
	}{
		{"no patches copies the file", nil, "0123456789"},
		{"zeroing keeps the size", []Patch{{Off: 2, Len: 3, Zero: true}}, "01\x00\x00\x0056789"},
		{"replacing with shorter data", []Patch{{Off: 2, Len: 3, Data: []byte("x")}}, "01x56789"},
		{"replacing with longer data", []Patch{{Off: 2, Len: 1, Data: []byte("xyz")}}, "01xyz3456789"},
		{"removing bytes", []Patch{{Off: 2, Len: 3}}, "0156789"},
		{"inserting bytes", []Patch{{Off: 5, Data: []byte("ab")}}, "01234ab56789"},
		{"patching the first byte", []Patch{{Off: 0, Len: 1, Data: []byte("x")}}, "x123456789"},
		{"patching the last byte", []Patch{{Off: 9, Len: 1, Data: []byte("x")}}, "012345678x"},
		{"appending at the end", []Patch{{Off: 10, Data: []byte("x")}}, "0123456789x"},
		{"truncating the tail", []Patch{{Off: 5, Len: 5}}, "01234"},
		{"replacing everything", []Patch{{Off: 0, Len: 10, Data: []byte("new")}}, "new"},
		{"adjacent patches", []Patch{{Off: 1, Len: 2, Data: []byte("a")}, {Off: 3, Len: 2, Data: []byte("b")}}, "0ab56789"},
		{"separate patches", []Patch{{Off: 1, Len: 1, Zero: true}, {Off: 8, Len: 1, Data: []byte("z")}}, "0\x00234567z9"},
		{"Zero wins over Data", []Patch{{Off: 0, Len: 2, Zero: true, Data: []byte("ignored")}}, "\x00\x0023456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := CopyPatched(context.Background(), &out, BytesSource(src), tt.patches); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCopyPatchedRejectsBadPatches(t *testing.T) {
	src := BytesSource([]byte("0123456789"))
	tests := []struct {
		name    string
		patches []Patch
	}{
		{"overlapping", []Patch{{Off: 0, Len: 5}, {Off: 4, Len: 1}}},
		{"out of order", []Patch{{Off: 5, Len: 1}, {Off: 1, Len: 1}}},
		{"negative offset", []Patch{{Off: -1, Len: 1}}},
		{"negative length", []Patch{{Off: 1, Len: -1}}},
		{"starts past the end", []Patch{{Off: 11}}},
		{"runs past the end", []Patch{{Off: 5, Len: 6}}},
		{"length that would overflow", []Patch{{Off: 5, Len: 1<<63 - 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CopyPatched(context.Background(), io.Discard, src, tt.patches)
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestCopyPatchedFailures(t *testing.T) {
	src := BytesSource([]byte("0123456789"))
	patches := []Patch{{Off: 2, Len: 1, Data: []byte("x")}}
	if err := CopyPatched(cancelled(), io.Discard, src, patches); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
	if err := CopyPatched(context.Background(), failingWriter{}, src, patches); !errors.Is(err, errWrite) {
		t.Errorf("failing writer: err = %v", err)
	}
	// Only the patch can fail here: nothing precedes it and it writes Data.
	err := CopyPatched(context.Background(), failingWriter{}, src, []Patch{{Off: 0, Len: 1, Data: []byte("x")}})
	if !errors.Is(err, errWrite) {
		t.Errorf("failing patch write: err = %v", err)
	}
}

func TestReadAll(t *testing.T) {
	data := sequence(100)
	got, err := ReadAll(BytesSource(data))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("ReadAll returned different bytes")
	}
	// The size check must come before the allocation, or this test would try to
	// allocate more than a gibibyte.
	if _, err := ReadAll(lyingSource{size: MaxInMemory + 1}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized source: err = %v, want ErrTooLarge", err)
	}
	if _, err := ReadAll(lyingSource{size: 8}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short read: err = %v", err)
	}
}

func TestNestedDepthLimit(t *testing.T) {
	opts := Options{Depth: MaxNestedDepth}
	if _, _, err := CleanBytes(context.Background(), []byte("x"), opts); !errors.Is(err, ErrTooLarge) {
		t.Errorf("CleanBytes at the depth limit: err = %v, want ErrTooLarge", err)
	}
	if _, err := InspectBytes(context.Background(), []byte("x"), opts); !errors.Is(err, ErrTooLarge) {
		t.Errorf("InspectBytes at the depth limit: err = %v, want ErrTooLarge", err)
	}
}

func TestErrorHelpersWrapSentinels(t *testing.T) {
	if err := Malformedf("bad %d", 1); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "bad 1") {
		t.Errorf("Malformedf = %v", err)
	}
	if err := Unsupportedf("bad %d", 2); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "bad 2") {
		t.Errorf("Unsupportedf = %v", err)
	}
}

func TestByteCount(t *testing.T) {
	if got := ByteCount(1); got != "1 byte" {
		t.Errorf("got %q", got)
	}
	if got := ByteCount(int64(0)); got != "0 bytes" {
		t.Errorf("got %q", got)
	}
	if got := ByteCount(int64(1 << 40)); got != "1099511627776 bytes" {
		t.Errorf("got %q", got)
	}
}

func TestAllZero(t *testing.T) {
	if !AllZero(nil) || !AllZero(make([]byte, 9)) {
		t.Error("zeros were not recognised")
	}
	if AllZero([]byte{0, 0, 1}) {
		t.Error("a non-zero byte was missed")
	}
}

func TestDecodeText(t *testing.T) {
	if got := DecodeText([]byte("caf\xc3\xa9")); got != "caf\u00e9" {
		t.Errorf("UTF-8: %q", got)
	}
	if got := DecodeText([]byte("caf\xe9")); got != "caf\u00e9" {
		t.Errorf("Latin-1: %q", got)
	}
	// A huge value is not converted whole, in either encoding.
	const limit = 4096
	if got := len([]rune(DecodeText(bytes.Repeat([]byte{0xE9}, 10*limit)))); got != limit {
		t.Errorf("Latin-1: decoded %d characters, want %d", got, limit)
	}
	if got := len(DecodeText(bytes.Repeat([]byte{'a'}, 10*limit))); got != limit {
		t.Errorf("ASCII: decoded %d bytes, want %d", got, limit)
	}
}

func TestIsZeroRange(t *testing.T) {
	data := make([]byte, 1000)
	data[700] = 1
	src := BytesSource(data)
	ctx := context.Background()
	buf := make([]byte, 64)
	for _, c := range []struct {
		off, n int64
		want   bool
	}{
		{0, 700, true},
		{0, 701, false},
		{700, 1, false},
		{701, 299, true},
		{0, 0, true},
		{1000, 0, true},
	} {
		got, err := IsZeroRange(ctx, src, c.off, c.n, buf)
		if err != nil || got != c.want {
			t.Errorf("%d+%d: got %t, %v", c.off, c.n, got, err)
		}
	}
	for _, c := range [][2]int64{{0, 1001}, {1001, 0}, {-1, 1}, {1, -1}} {
		if _, err := IsZeroRange(ctx, src, c[0], c[1], buf); !errors.Is(err, ErrMalformed) {
			t.Errorf("%v: got %v", c, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := IsZeroRange(canceled, src, 0, 10, buf); !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation: got %v", err)
	}
}
