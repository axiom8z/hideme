package exif

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// byteOrder is what the hand-placed blocks below need from a binary order.
type byteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

var bothOrders = []byteOrder{binary.LittleEndian, binary.BigEndian}

// rawEntry is a directory entry whose value field is given directly. The hostile
// cases are all about offsets and counts that coretest's encoder would correct,
// so those blocks are placed by hand.
type rawEntry struct {
	tag, typ uint16
	count    uint32
	value    uint32
}

func rawHeader(order byteOrder, first uint32) []byte {
	b := []byte("II")
	if order.String() == binary.BigEndian.String() {
		b = []byte("MM")
	}
	b = order.AppendUint16(b, tiffMagic)
	return order.AppendUint32(b, first)
}

func appendIFD(b []byte, order byteOrder, next uint32, entries ...rawEntry) []byte {
	b = order.AppendUint16(b, uint16(len(entries)))
	for _, e := range entries {
		b = order.AppendUint16(b, e.tag)
		b = order.AppendUint16(b, e.typ)
		b = order.AppendUint32(b, e.count)
		b = order.AppendUint32(b, e.value)
	}
	return order.AppendUint32(b, next)
}

// ifdSize is the length appendIFD adds for a directory of n entries.
func ifdSize(n int) uint32 { return uint32(2 + entrySize*n + 4) }

// pointer is a sub-IFD link in the form every real writer uses.
func pointer(tag uint16, off uint32) rawEntry {
	return rawEntry{tag: tag, typ: typeLong, count: 1, value: off}
}

func findField(findings []core.Finding, field string) (core.Finding, bool) {
	i := slices.IndexFunc(findings, func(f core.Finding) bool { return f.Field == field })
	if i < 0 {
		return core.Finding{}, false
	}
	return findings[i], true
}

func fieldNames(findings []core.Finding) []string {
	names := make([]string, len(findings))
	for i, f := range findings {
		names[i] = f.Field
	}
	return names
}

func mustParse(t *testing.T, block []byte) *Data {
	t.Helper()
	d, err := Parse(block)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func residualCount(findings []core.Finding) int {
	n := 0
	for _, f := range findings {
		if f.Action != core.ActionRetain {
			n++
		}
	}
	return n
}

// checkBlock asserts everything that must hold for any bytes at all. It is shared
// by the truncation and corruption sweeps and by FuzzParse.
func checkBlock(t *testing.T, block []byte) {
	t.Helper()
	d, err := Parse(block)
	if d == nil {
		t.Fatal("Parse returned a nil Data")
	}
	if err != nil && !errors.Is(err, core.ErrMalformed) {
		t.Fatalf("Parse error does not wrap ErrMalformed: %v", err)
	}
	orientation := d.Orientation()
	if orientation > 8 {
		t.Errorf("Orientation = %d, outside 0 through 8", orientation)
	}
	for _, opts := range []core.Options{{}, {Strict: true}} {
		findings := d.Findings("W", opts)
		checkFindings(t, findings)
		if !slices.Equal(findings, d.Findings("W", opts)) {
			t.Error("Findings is not deterministic")
		}

		keep := d.Replacement(opts)
		if opts.Strict && keep != nil {
			t.Error("strict mode offers a replacement")
		}
		if keep != nil && !IsMinimal(keep) {
			t.Errorf("Replacement %x is not minimal", keep)
		}
		retained := len(findings) - residualCount(findings)
		if (keep != nil) != (retained > 0) {
			t.Errorf("Replacement is %x but %d findings are retained", keep, retained)
		}

		// The rule that makes verification trustworthy: a block reports nothing to
		// remove only if it is exactly what Clean would have written.
		cleanAlready := bytes.Equal(StripPrefix(block), keep) && err == nil
		if residual := residualCount(findings); cleanAlready != (residual == 0) {
			t.Errorf("strict=%v: %d findings to remove, but block already clean = %v", opts.Strict, residual, cleanAlready)
		}

		if keep != nil {
			again := mustParse(t, keep)
			if !bytes.Equal(again.Replacement(opts), keep) {
				t.Error("Replacement is not idempotent")
			}
			if n := residualCount(again.Findings("W", opts)); n != 0 {
				t.Errorf("Replacement still has %d findings to remove", n)
			}
		}
	}
}

func checkFindings(t *testing.T, findings []core.Finding) {
	t.Helper()
	for _, f := range findings {
		if f.Field == "" || f.Category == "" || f.Risk == "" || f.Action == "" {
			t.Errorf("incomplete finding: %+v", f)
		}
		if f.Action == core.ActionRetain && f.Reason == "" {
			t.Errorf("retained without a reason: %+v", f)
		}
		for _, s := range []string{f.Field, f.Value, f.Where, f.Reason} {
			if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
				return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
			}) {
				t.Errorf("unsafe text %q in %+v", s, f)
			}
			if n := utf8.RuneCountInString(s); n > 203 {
				t.Errorf("%d characters in %+v", n, f)
			}
		}
	}
}

func TestStripPrefix(t *testing.T) {
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"JPEG segment", append([]byte("Exif\x00\x00"), tiff...), tiff},
		{"bare TIFF", tiff, tiff},
		{"only the prefix", []byte("Exif\x00\x00"), []byte{}},
		{"prefix with a single NUL is not the prefix", []byte("Exif\x00II"), []byte("Exif\x00II")},
		{"only one prefix is removed", []byte("Exif\x00\x00Exif\x00\x00"), []byte("Exif\x00\x00")},
		{"empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripPrefix(tt.in); !bytes.Equal(got, tt.want) {
				t.Errorf("StripPrefix(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidStructure covers the layouts real files use that a stricter parser
// could mistake for damage.
func TestValidStructure(t *testing.T) {
	for _, order := range bothOrders {
		t.Run(order.String(), func(t *testing.T) {
			t.Run("no next pointer at the very end of the block", func(t *testing.T) {
				block := appendIFD(rawHeader(order, headerSize), order, 0, rawEntry{tag: 0x0100, typ: typeLong, count: 1, value: 1})
				block = block[:len(block)-4]
				mustParse(t, block)
			})
			t.Run("zero sub-IFD pointer means absent", func(t *testing.T) {
				block := appendIFD(rawHeader(order, headerSize), order, 0, pointer(tagExifIFD, 0), pointer(tagGPSIFD, 0))
				if d := mustParse(t, block); len(d.dirs) != 1 {
					t.Errorf("got %d directories, want just IFD0", len(d.dirs))
				}
			})
			t.Run("sub-IFD with garbage in its next field is not damage", func(t *testing.T) {
				block := appendIFD(rawHeader(order, headerSize), order, 0, pointer(tagGPSIFD, headerSize+ifdSize(1)))
				block = appendIFD(block, order, 0xDEADBEEF)
				if d := mustParse(t, block); len(d.dirs) != 2 {
					t.Errorf("got %d directories, want IFD0 and GPS", len(d.dirs))
				}
			})
			t.Run("links out of GPS and Interop IFDs are not followed", func(t *testing.T) {
				// These tag numbers mean something else in those directories, so a
				// value that happens to look like a back-link is not a cycle.
				block := appendIFD(rawHeader(order, headerSize), order, 0, pointer(tagGPSIFD, headerSize+ifdSize(1)))
				block = appendIFD(block, order, 0, pointer(tagExifIFD, headerSize), pointer(tagGPSIFD, headerSize), pointer(tagInteropIFD, headerSize))
				d := mustParse(t, block)
				if len(d.dirs) != 2 {
					t.Errorf("got %d directories, want IFD0 and GPS", len(d.dirs))
				}
				if f, _ := findField(d.Findings("W", core.Options{}), "Other GPS tags"); f.Value != "3 tags" {
					t.Errorf("catch-all = %q", f.Value)
				}
			})
			t.Run("sub-IFD pointer stored as a SHORT", func(t *testing.T) {
				// A SHORT sits in the first two bytes of the value field, whatever the order.
				value := uint32(headerSize + ifdSize(1))
				if order.String() == binary.BigEndian.String() {
					value <<= 16
				}
				block := appendIFD(rawHeader(order, headerSize), order, 0, rawEntry{tag: tagGPSIFD, typ: typeShort, count: 1, value: value})
				block = appendIFD(block, order, 0)
				if d := mustParse(t, block); len(d.dirs) != 2 {
					t.Errorf("got %d directories, want IFD0 and GPS", len(d.dirs))
				}
			})
			t.Run("sub-IFD pointer of the IFD type", func(t *testing.T) {
				block := appendIFD(rawHeader(order, headerSize), order, 0, rawEntry{tag: tagGPSIFD, typ: typeIFD, count: 1, value: headerSize + ifdSize(1)})
				block = appendIFD(block, order, 0)
				if d := mustParse(t, block); len(d.dirs) != 2 {
					t.Errorf("got %d directories, want IFD0 and GPS", len(d.dirs))
				}
			})
			t.Run("GPS linked from the Exif IFD and Interop from IFD0", func(t *testing.T) {
				root := &coretest.IFD{
					Exif:    &coretest.IFD{GPS: &coretest.IFD{Entries: []coretest.Entry{{Tag: gpsLatitudeRef, Value: "N"}}}},
					Interop: &coretest.IFD{Entries: []coretest.Entry{{Tag: 1, Value: "R98"}}},
				}
				d := mustParse(t, root.Encode(order))
				var names []string
				for _, dir := range d.dirs {
					names = append(names, dir.name)
				}
				if want := []string{"IFD0", "Exif", "GPS", "Interop"}; !slices.Equal(names, want) {
					t.Errorf("directories = %q, want %q", names, want)
				}
			})
			t.Run("long IFD chain within the cap", func(t *testing.T) {
				if d := mustParse(t, ifdChain(order, maxIFDs)); len(d.dirs) != maxIFDs {
					t.Errorf("got %d directories, want %d", len(d.dirs), maxIFDs)
				}
			})
			t.Run("entry count at the per-IFD cap", func(t *testing.T) {
				block := appendIFD(rawHeader(order, headerSize), order, 0, fillerEntries(maxEntriesPerIFD)...)
				if d := mustParse(t, block); len(d.dirs[0].entries) != maxEntriesPerIFD {
					t.Errorf("got %d entries", len(d.dirs[0].entries))
				}
			})
		})
	}
}

// ifdChain returns n empty directories, each linked to the next.
func ifdChain(order byteOrder, n int) []byte {
	block := rawHeader(order, headerSize)
	for i := range n {
		var next uint32
		if i < n-1 {
			next = headerSize + uint32(i+1)*ifdSize(0)
		}
		block = appendIFD(block, order, next)
	}
	return block
}

func fillerEntries(n int) []rawEntry {
	entries := make([]rawEntry, n)
	for i := range entries {
		entries[i] = rawEntry{tag: 0x0100, typ: typeLong, count: 1, value: 1}
	}
	return entries
}

// hostileBlocks lists blocks that must fail to parse, each with a word its error
// must contain. They are built for one byte order and run in both.
func hostileBlocks(order byteOrder) map[string]struct {
	block []byte
	want  string
} {
	type hostile = struct {
		block []byte
		want  string
	}
	header := rawHeader(order, headerSize)
	single := func(entries ...rawEntry) []byte {
		return appendIFD(bytes.Clone(header), order, 0, entries...)
	}

	// Two more directories after a one-entry IFD0, for the sub-IFD cycles.
	second := headerSize + ifdSize(1)

	cases := map[string]hostile{
		// Cycles.
		"IFD0 next points at itself": {appendIFD(bytes.Clone(header), order, headerSize), "cycle"},
		"IFD1 next points back at IFD0": {
			appendIFD(appendIFD(bytes.Clone(header), order, headerSize+ifdSize(0)), order, headerSize),
			"cycle",
		},
		"Exif pointer back at IFD0": {single(pointer(tagExifIFD, headerSize)), "cycle"},
		"GPS pointer back at IFD0":  {single(pointer(tagGPSIFD, headerSize)), "cycle"},
		"Interop pointer back at IFD0": {
			single(pointer(tagInteropIFD, headerSize)), "cycle",
		},
		"Exif pointer at itself": {
			appendIFD(single(pointer(tagExifIFD, second)), order, 0, pointer(tagExifIFD, second)),
			"cycle",
		},
		"two Exif IFDs point at each other": {
			appendIFD(appendIFD(single(pointer(tagExifIFD, second)), order, 0, pointer(tagExifIFD, second+ifdSize(1))),
				order, 0, pointer(tagExifIFD, second)),
			"cycle",
		},
		"the IFD chain revisits a directory already read as the Exif IFD": {
			appendIFD(appendIFD(bytes.Clone(header), order, second, pointer(tagExifIFD, second)), order, 0),
			"cycle",
		},
		"GPS and Exif share a directory": {
			appendIFD(appendIFD(bytes.Clone(header), order, 0,
				pointer(tagExifIFD, headerSize+ifdSize(2)),
				pointer(tagGPSIFD, headerSize+ifdSize(2))), order, 0),
			"cycle",
		},

		// Offsets past the end, including ones that wrap a 32-bit addition.
		"IFD0 past the end":      {rawHeader(order, 1000), "past the end"},
		"IFD0 at the very end":   {rawHeader(order, headerSize), "past the end"},
		"IFD0 at the max offset": {rawHeader(order, 0xFFFFFFFF), "past the end"},
		"IFD1 past the end":      {appendIFD(bytes.Clone(header), order, 0xFFFFFFF0), "past the end"},
		"Exif pointer past the end": {
			single(pointer(tagExifIFD, 0xFFFFFFFF)), "past the end",
		},
		"GPS pointer past the end": {single(pointer(tagGPSIFD, 1<<20)), "past the end"},
		"Exif pointer into the header": {
			single(pointer(tagExifIFD, headerSize-1)), "into the header",
		},
		"value offset past the end": {
			single(rawEntry{tag: 0x010F, typ: typeASCII, count: 16, value: 1 << 20}), "overruns",
		},
		"value offset that wraps a 32-bit addition": {
			single(rawEntry{tag: 0x010F, typ: typeASCII, count: 32, value: 0xFFFFFFF0}), "overruns",
		},
		"value one byte past the end": {
			single(rawEntry{tag: 0x010F, typ: typeASCII, count: ifdSize(1) + 1, value: headerSize}), "overruns",
		},

		// Huge counts, including ones whose byte size wraps 32 bits.
		"maximum count": {
			single(rawEntry{tag: 0x010F, typ: typeASCII, count: 0xFFFFFFFF, value: headerSize}), "overruns",
		},
		"count times eight wraps to zero": {
			single(rawEntry{tag: 0x927C, typ: typeRational, count: 1 << 29, value: headerSize}), "overruns",
		},
		"maximum count of doubles": {
			single(rawEntry{tag: 0x927C, typ: typeDouble, count: 0xFFFFFFFF, value: 0xFFFFFFFF}), "overruns",
		},

		// Too much structure.
		"entry count over the per-IFD cap": {
			appendIFD(bytes.Clone(header), order, 0, fillerEntries(maxEntriesPerIFD+1)...), "over the limit",
		},
		"maximum entry count with no entries": {
			append(bytes.Clone(header), countOnly(order, 0xFFFF)...), "over the limit",
		},
		"entry table overruns the block": {
			append(bytes.Clone(header), countOnly(order, 100)...), "overruns",
		},
		"entry table cut mid-entry": {
			single(fillerEntries(2)...)[:headerSize+2+entrySize+5], "overruns",
		},
		"more IFDs than the cap": {ifdChain(order, maxIFDs+1), "IFDs"},
		"more sub-IFD pointers than the cap": {
			manySubIFDs(order, maxIFDs+4), "IFDs",
		},
		"more entries in total than the cap": {manyEntries(order), "in total"},

		// Pointers that cannot be read.
		"Exif pointer stored as text": {
			single(rawEntry{tag: tagExifIFD, typ: typeASCII, count: 4, value: headerSize}), "not a valid pointer",
		},
		"GPS pointer with a count of two": {
			single(rawEntry{tag: tagGPSIFD, typ: typeLong, count: 2, value: headerSize}), "not a valid pointer",
		},
		"Interop pointer of an unknown type": {
			single(rawEntry{tag: tagInteropIFD, typ: 99, count: 1, value: headerSize}), "not a valid pointer",
		},
	}
	return cases
}

func countOnly(order byteOrder, count uint16) []byte {
	return order.AppendUint16(nil, count)
}

// manySubIFDs returns an IFD0 linking to n distinct empty directories.
func manySubIFDs(order byteOrder, n int) []byte {
	entries := make([]rawEntry, n)
	first := headerSize + ifdSize(n)
	for i := range entries {
		entries[i] = pointer(tagExifIFD, first+uint32(i)*ifdSize(0))
	}
	block := appendIFD(rawHeader(order, headerSize), order, 0, entries...)
	for range n {
		block = appendIFD(block, order, 0)
	}
	return block
}

// manyEntries returns a chain of directories, each legal, that together have
// more entries than Parse accepts.
func manyEntries(order byteOrder) []byte {
	const perIFD = maxEntriesPerIFD
	const ifds = maxTotalEntries/perIFD + 1
	block := rawHeader(order, headerSize)
	for i := range ifds {
		var next uint32
		if i < ifds-1 {
			next = headerSize + uint32(i+1)*ifdSize(perIFD)
		}
		block = appendIFD(block, order, next, fillerEntries(perIFD)...)
	}
	return block
}

func TestHostileBlocksFailClosed(t *testing.T) {
	for _, order := range bothOrders {
		for name, tt := range hostileBlocks(order) {
			t.Run(order.String()+"/"+name, func(t *testing.T) {
				d, err := Parse(tt.block)
				if !errors.Is(err, core.ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("err = %q, want it to mention %q", err, tt.want)
				}
				f, ok := findField(d.Findings("APP1", core.Options{}), "Damaged EXIF block")
				if !ok {
					t.Fatalf("no damage finding in %q", fieldNames(d.Findings("APP1", core.Options{})))
				}
				if f.Action != core.ActionRemove || f.Risk != core.RiskHigh || f.Category != core.CatHidden {
					t.Errorf("damage finding = %+v", f)
				}
				if d.Orientation() != 0 || d.Replacement(core.Options{}) != nil {
					t.Error("no orientation can be read from this block")
				}
				checkBlock(t, tt.block)
			})
		}
	}
}

func TestMalformedHeaders(t *testing.T) {
	le := binary.LittleEndian
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, "too short"},
		{"one byte", []byte("I"), "too short"},
		{"seven bytes", []byte("II*\x00\x08\x00\x00"), "too short"},
		{"prefix left on", append([]byte(Prefix), Minimal(6)...), "byte order"},
		{"bad byte order mark", []byte("XX*\x00\x08\x00\x00\x00"), "byte order"},
		{"mixed byte order mark", []byte("IM*\x00\x08\x00\x00\x00"), "byte order"},
		{"wrong magic", []byte("II\x2B\x00\x08\x00\x00\x00"), "magic"},
		{"magic in the wrong order", []byte("MM*\x00\x00\x00\x00\x08"), "magic"},
		{"IFD0 offset of zero", rawHeader(le, 0), "into the header"},
		{"IFD0 offset inside the header", rawHeader(le, headerSize-1), "into the header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Parse(tt.in)
			if !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
			if d == nil {
				t.Fatal("nil Data")
			}
			if _, ok := findField(d.Findings("", core.Options{}), "Damaged EXIF block"); !ok {
				t.Error("no damage finding")
			}
			checkBlock(t, tt.in)
		})
	}
}

func TestErrorTextCannotInjectControls(t *testing.T) {
	_, err := Parse([]byte("\x1b[..*\x00\x08\x00\x00\x00"))
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err = %q", err)
	}
}

// TestDamageKeepsWhatCouldBeRead is why Parse returns a Data alongside its error:
// a block with one bad value should still tell the user about its GPS position.
func TestDamageKeepsWhatCouldBeRead(t *testing.T) {
	for _, order := range bothOrders {
		t.Run(order.String(), func(t *testing.T) {
			root := coretest.FakeIFD(6)
			root.Exif.Entries = append(root.Exif.Entries, coretest.Entry{
				Tag: 0xA40B, Value: []byte("placeholder"), Count: 0x7FFFFFFF,
			})
			block := root.Encode(order)
			d, err := Parse(block)
			if !errors.Is(err, core.ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
			findings := d.Findings("W", core.Options{})
			for _, field := range []string{"GPS coordinates", "Artist", "Camera model", "MakerNote", "Embedded thumbnail", "Damaged EXIF block"} {
				if _, ok := findField(findings, field); !ok {
					t.Errorf("no %q in %q", field, fieldNames(findings))
				}
			}
			// Viewers still read the orientation of such a block, so dropping it would
			// turn the picture sideways, and a value from 1 to 8 can carry nothing else.
			if got, _ := findField(findings, "Orientation"); got.Action != core.ActionRetain {
				t.Errorf("orientation from a damaged block has action %q", got.Action)
			}
			if got := d.Replacement(core.Options{}); !bytes.Equal(got, Minimal(6)) {
				t.Errorf("Replacement = %x", got)
			}
			if got := d.Replacement(core.Options{Strict: true}); got != nil {
				t.Errorf("strict Replacement = %x", got)
			}
		})
	}
}

func TestEveryTruncationIsSafe(t *testing.T) {
	for _, order := range bothOrders {
		block := coretest.FakeIFD(6).Encode(order)
		for n := range len(block) + 1 {
			checkBlock(t, block[:n])
		}
		if t.Failed() {
			t.Fatalf("%v: a truncated block broke an invariant", order)
		}
	}
}

func TestEveryByteCorruptionIsSafe(t *testing.T) {
	for _, order := range bothOrders {
		block := coretest.FakeIFD(6).Encode(order)
		mutated := bytes.Clone(block)
		for i := range block {
			for _, value := range []byte{0x00, 0x01, 0x7F, 0x80, 0xFF, block[i] ^ 0xFF, block[i] + 1} {
				mutated[i] = value
				checkBlock(t, mutated)
			}
			mutated[i] = block[i]
			if t.Failed() {
				t.Fatalf("%v: corrupting byte %d broke an invariant", order, i)
			}
		}
	}
}

// TestRepeatedPointersToOneRegionStayBounded has thousands of entries claiming
// the same large value, the shape that would make a careless decoder quadratic.
func TestRepeatedPointersToOneRegionStayBounded(t *testing.T) {
	order := binary.LittleEndian
	const entries = 4000
	total := headerSize + ifdSize(entries)
	payload := bytes.Repeat([]byte("A"), 1<<20)
	list := make([]rawEntry, entries)
	for i := range list {
		list[i] = rawEntry{tag: 0x013B, typ: typeASCII, count: uint32(len(payload)), value: total}
	}
	block := append(appendIFD(rawHeader(order, headerSize), order, 0, list...), payload...)
	d := mustParse(t, block)
	findings := d.Findings("W", core.Options{})
	checkFindings(t, findings)
	if len(findings) != 2 {
		t.Errorf("got fields %q, want Artist and the catch-all count", fieldNames(findings))
	}
	if f, _ := findField(findings, "Other EXIF tags"); f.Value != "3999 tags" {
		t.Errorf("catch-all = %q", f.Value)
	}
}

// Errors reach the user, and these bytes are text from the file.
func TestErrorsDoNotRepeatTextFromTheFile(t *testing.T) {
	if _, err := Parse([]byte("QZ*\x00\x08\x00\x00\x00")); err == nil || strings.Contains(err.Error(), "QZ") {
		t.Errorf("err = %q", err)
	}
}
