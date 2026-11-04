package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/exif"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

const hiddenText = "Test Hidden Text"

func hiddenTable(id byte) []byte {
	return append([]byte{id}, bytes.Repeat([]byte(hiddenText), 4)...) // 1 + 64 bytes
}

func assertNoHiddenText(t *testing.T, out []byte) {
	t.Helper()
	if bytes.Contains(out, []byte(hiddenText)) || bytes.Contains(out, []byte("Test")) {
		t.Fatal("the hidden text survived cleaning")
	}
}

func assertIdempotent(t *testing.T, out []byte) {
	t.Helper()
	again, rep := clean(t, out, core.Options{})
	if !bytes.Equal(again, out) {
		t.Fatal("cleaning the output changed it")
	}
	if residual := rep.Residual(); len(residual) != 0 {
		t.Fatalf("residual findings: %+v", residual)
	}
}

func refuses(t *testing.T, data []byte, want error) {
	t.Helper()
	var out bytes.Buffer
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// A table no scan refers to is never read by anything, and it holds 64 or more bytes
// of whatever a writer likes, as many times as it likes. Noise hides in tables too,
// but at least not in the ones a decoder skips.
func TestUnusedTablesAreRemoved(t *testing.T) {
	base := noisyJPEG(t)
	dht := append([]byte{0x13}, make([]byte, 16)...)
	dht[8] = byte(len(hiddenText))
	dht = append(dht, hiddenText...)
	cases := map[string][]byte{
		"DQT with an id no component uses": insertAfterSOI(base, segment(jpegmeta.MarkerDQT, hiddenTable(3))),
		"DQT repeated many times":          insertAfterSOI(base, slices.Repeat([][]byte{segment(jpegmeta.MarkerDQT, hiddenTable(3))}, 50)...),
		"DQT overwritten before any scan":  insertAfterSOI(base, segment(jpegmeta.MarkerDQT, hiddenTable(0))),
		"DHT with an id no scan uses":      insertAfterSOI(base, segment(jpegmeta.MarkerDHT, dht)),
		"DRI overwritten before any scan":  insertAfterSOI(base, segment(jpegmeta.MarkerDRI, []byte("Te")), segment(jpegmeta.MarkerDRI, []byte("st"))),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			before := decode(t, data)
			out, rep := clean(t, data, core.Options{})
			assertNoHiddenText(t, out)
			assertSamePixels(t, before, decode(t, out))
			if !bytes.Contains(out, lastScan(t, base)) {
				t.Fatal("the scan was lost")
			}
			if !hasField(rep, "Unused decoding tables", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			assertIdempotent(t, out)
		})
	}
}

// Tables a scan uses stay, even in a segment shared with a table none uses, and a
// file with nothing unused passes through untouched.
func TestUsedTablesAreKept(t *testing.T) {
	base := noisyJPEG(t)
	if out, rep := clean(t, base, core.Options{}); !bytes.Equal(out, base) || len(rep.Findings) != 0 {
		t.Fatalf("a file with no metadata must not change: %+v", rep.Findings)
	}
	progressive := progressiveJPEG(noBetween)
	if out, rep := clean(t, progressive, core.Options{}); !bytes.Equal(out, progressive) || len(rep.Findings) != 0 {
		t.Fatalf("every table of a progressive file is used: %+v", rep.Findings)
	}
	restart := restartJPEG(false)
	if out, rep := clean(t, restart, core.Options{}); !bytes.Equal(out, restart) || len(rep.Findings) != 0 {
		t.Fatalf("every table of a restart file is used: %+v", rep.Findings)
	}

	// Table 0 and an unused table 3 share one DQT segment.
	used := append([]byte{0}, bytes.Repeat([]byte{4}, 64)...)
	shared := bytes.Replace(restart, segment(jpegmeta.MarkerDQT, used), segment(jpegmeta.MarkerDQT, used, hiddenTable(3)), 1)
	if bytes.Equal(shared, restart) {
		t.Fatal("the fixture did not change")
	}
	out, _ := clean(t, shared, core.Options{})
	if !bytes.Equal(out, restart) {
		t.Fatal("only the unused table should have gone")
	}
}

// A scan after a redefinition uses the new table and earlier scans the old one, and
// image/jpeg and libjpeg read a table's last definition at different moments, so
// each definition in effect at a scan or at the end of the file has to stay.
func TestTablesRedefinedBetweenScansAreKept(t *testing.T) {
	second := segment(jpegmeta.MarkerDQT, append([]byte{0}, bytes.Repeat([]byte{9}, 64)...))
	third := segment(jpegmeta.MarkerDQT, append([]byte{0}, bytes.Repeat([]byte{7}, 64)...))
	data := progressiveJPEG(func(scan int) []byte {
		if scan == 1 {
			return second
		}
		return nil
	})
	eoi := len(data) - 2
	data = slices.Concat(data[:eoi], third, data[eoi:])
	out, rep := clean(t, data, core.Options{})
	if !bytes.Equal(out, data) || len(rep.Findings) != 0 {
		t.Fatalf("output differs, findings %+v", rep.Findings)
	}
}

// DAC conditions the arithmetic decoder, so in a Huffman file nothing reads it, and
// even in an arithmetic file each value has a defined range.
func TestArithmeticConditioningIsChecked(t *testing.T) {
	base := noisyJPEG(t)
	t.Run("removed from a Huffman file", func(t *testing.T) {
		out, rep := clean(t, insertAfterSOI(base, segment(jpegmeta.MarkerDAC, []byte{0x10, 5, 0x00, 0x10})), core.Options{})
		if !bytes.Equal(out, base) || !hasField(rep, "Unused decoding tables", core.ActionRemove) {
			t.Fatalf("findings: %+v", rep.Findings)
		}
	})
	arithmetic := arithmeticJPEG(segment(jpegmeta.MarkerDAC, []byte{0x00, 0x10, 0x10, 5}))
	t.Run("kept in an arithmetic file", func(t *testing.T) {
		if out, rep := clean(t, arithmetic, core.Options{}); !bytes.Equal(out, arithmetic) || len(rep.Findings) != 0 {
			t.Fatalf("findings: %+v", rep.Findings)
		}
	})
	for name, payload := range map[string][]byte{
		"text":               []byte(hiddenText),
		"table id too high":  {0x04, 0x10},
		"class too high":     {0x20, 0x10},
		"AC value of zero":   {0x10, 0},
		"AC value too high":  {0x10, 64},
		"DC with low > high": {0x00, 0x01},
		"odd length":         {0x00},
		"empty":              {},
	} {
		t.Run(name, func(t *testing.T) {
			refuses(t, arithmeticJPEG(segment(jpegmeta.MarkerDAC, payload)), core.ErrMalformed)
		})
	}
}

func TestAdobeSegmentKeepsOnlyTheTransform(t *testing.T) {
	base := noisyJPEG(t)
	canonical := func(transform byte) []byte {
		return segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe), []byte{0, 100, 0, 0, 0, 0, transform})
	}
	hostile := segment(jpegmeta.MarkerAPP0+14, []byte(sigAdobe), []byte("Test!!"), []byte{2})
	t.Run("free fields are replaced", func(t *testing.T) {
		out, rep := clean(t, insertAfterSOI(base, hostile), core.Options{})
		assertNoHiddenText(t, out)
		if !bytes.Contains(out, canonical(2)) {
			t.Fatal("the transform was lost")
		}
		if !hasField(rep, "Adobe header values", core.ActionAnonymize) {
			t.Fatalf("no finding: %+v", rep.Findings)
		}
		assertIdempotent(t, out)
	})
	t.Run("repeats are dropped", func(t *testing.T) {
		out, rep := clean(t, insertAfterSOI(base, canonical(2), hostile, hostile), core.Options{})
		assertNoHiddenText(t, out)
		if got := bytes.Count(out, []byte(sigAdobe)); got != 1 {
			t.Fatalf("%d Adobe segments", got)
		}
		if !hasField(rep, "Stray Adobe segment", core.ActionRemove) {
			t.Fatalf("no finding: %+v", rep.Findings)
		}
	})
	t.Run("after the image data it does nothing", func(t *testing.T) {
		eoi := len(base) - 2
		out, rep := clean(t, slices.Concat(base[:eoi], canonical(1), base[eoi:]), core.Options{})
		if !bytes.Equal(out, base) || !hasField(rep, "Stray Adobe segment", core.ActionRemove) {
			t.Fatalf("findings: %+v", rep.Findings)
		}
	})
	t.Run("a disagreement cannot be settled", func(t *testing.T) {
		refuses(t, insertAfterSOI(base, canonical(1), canonical(2)), core.ErrMalformed)
	})
}

// Only the first DNL matters, and only when the frame left its height to one.
func TestNumberOfLinesIsKeptOnlyWhereItMeansSomething(t *testing.T) {
	base := noisyJPEG(t)
	eoi := len(base) - 2
	out, rep := clean(t, slices.Concat(base[:eoi], segment(jpegmeta.MarkerDNL, []byte("Te")), base[eoi:]), core.Options{})
	if !bytes.Equal(out, base) || !hasField(rep, "Unused decoding tables", core.ActionRemove) {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	zero := bytes.Replace(restartJPEG(false), []byte{0xFF, 0xC0, 0, 11, 8, 0, 16}, []byte{0xFF, 0xC0, 0, 11, 8, 0, 0}, 1)
	dnl := segment(jpegmeta.MarkerDNL, []byte{0, 16})
	eoi = len(zero) - 2
	data := slices.Concat(zero[:eoi], dnl, segment(jpegmeta.MarkerDNL, []byte("Te")), zero[eoi:])
	out, _ = clean(t, data, core.Options{})
	if want := slices.Concat(zero[:eoi], dnl, zero[eoi:]); !bytes.Equal(out, want) {
		t.Fatal("exactly the first DNL should stay")
	}
}

// libjpeg and image/jpeg both step over bytes between segments with a warning, so
// such a file displays. The same search for the next marker here finds the same
// segments they do, and the stray bytes themselves are not copied.
func TestStrayBytesBetweenSegmentsAreDropped(t *testing.T) {
	base := noisyJPEG(t)
	comment := segment(jpegmeta.MarkerCOM, []byte("x"))
	for name, stray := range map[string][]byte{
		"text":       []byte(hiddenText),
		"line break": []byte("\r\n"),
		"zero bytes": {0, 0, 0, 0},
		"stuffed FF": {0xFF, 0x00},
		"mixed":      append([]byte("Test"), 0xFF, 0x00, 'x'),
	} {
		t.Run(name, func(t *testing.T) {
			data := insertAfterSOI(base, comment, stray)
			before := decode(t, data)
			out, rep := clean(t, data, core.Options{})
			assertNoHiddenText(t, out)
			if !bytes.Equal(out, base) {
				t.Fatal("the output should be the plain image")
			}
			assertSamePixels(t, before, decode(t, out))
			if !hasField(rep, "Stray bytes", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
		})
	}
	t.Run("fill bytes before a stuffed zero", func(t *testing.T) {
		// libjpeg steps over these and image/jpeg refuses them, so only the bytes can be compared.
		out, _ := clean(t, insertAfterSOI(base, comment, append([]byte{0xFF, 0xFF, 0x00}, hiddenText...)), core.Options{})
		if !bytes.Equal(out, base) {
			t.Fatal("the output should be the plain image")
		}
	})
	t.Run("a segment hiding behind them is still seen", func(t *testing.T) {
		out, rep := clean(t, insertAfterSOI(base, comment, []byte("xx"), segment(jpegmeta.MarkerAPP0+5, []byte(hiddenText))), core.Options{})
		assertNoHiddenText(t, out)
		if !hasField(rep, "APP5 segment", core.ActionRemove) {
			t.Fatalf("no finding: %+v", rep.Findings)
		}
	})
}

// Cleaning takes a real part of some photos away. Listing that as a finding among
// dozens is not enough, because the user's photo changes for good.
func TestLostPicturesAreAnnounced(t *testing.T) {
	base := noisyJPEG(t)
	video := "\x00\x00\x00\x18ftypmp42" + strings.Repeat("\x01", 40)
	mpf := segment(jpegmeta.MarkerAPP0+2, []byte(sigMPF), []byte("MM\x00*"))
	cases := []struct {
		name string
		data []byte
		want []string
	}{
		{"motion photo video", append(bytes.Clone(base), video...), []string{"video"}},
		{"video behind a marker", append(bytes.Clone(base), "\x00\x00MotionPhoto_Data"+video...), []string{"video"}},
		{"Samsung marker alone", append(bytes.Clone(base), "\x00\x00MotionPhoto_Data"...), []string{"video"}},
		{"further image", append(bytes.Clone(base), base...), []string{"another JPEG image"}},
		{"gain map and video", slices.Concat(base, base, []byte(video)), []string{"another JPEG image", "video"}},
		{"unknown data", append(bytes.Clone(base), "Test Unknown"...), []string{"bytes after the end of the image"}},
		{"pictures an index lists", append(insertAfterSOI(base, mpf), base...), []string{"another JPEG image", "multi-picture index"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rep := clean(t, tc.data, core.Options{})
			inspected, err := handler{}.Inspect(context.Background(), core.BytesSource(tc.data), core.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rep.Warnings, inspected.Warnings) {
				t.Fatalf("Clean warns %q and Inspect warns %q", rep.Warnings, inspected.Warnings)
			}
			if len(rep.Warnings) != 1 {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
			// --in-place must not destroy these unless told to.
			if want := tc.name != "unknown data"; rep.MayLoseContent != want || inspected.MayLoseContent != want {
				t.Errorf("MayLoseContent = %v and %v, want %v", rep.MayLoseContent, inspected.MayLoseContent, want)
			}
			for _, want := range tc.want {
				if !strings.Contains(rep.Warnings[0], want) {
					t.Errorf("warning %q does not mention %q", rep.Warnings[0], want)
				}
			}
		})
	}
	t.Run("an index with nothing after the image loses nothing", func(t *testing.T) {
		_, rep := clean(t, insertAfterSOI(base, mpf), core.Options{})
		if len(rep.Warnings) != 0 {
			t.Fatalf("warnings = %q", rep.Warnings)
		}
	})
	t.Run("zero padding loses nothing", func(t *testing.T) {
		_, rep := clean(t, append(bytes.Clone(base), make([]byte, 100)...), core.Options{})
		if len(rep.Warnings) != 0 {
			t.Fatalf("warnings = %q", rep.Warnings)
		}
	})
	t.Run("a plain file gets no warning", func(t *testing.T) {
		if _, rep := clean(t, base, core.Options{}); len(rep.Warnings) != 0 {
			t.Fatalf("warnings = %q", rep.Warnings)
		}
	})
}

// A photo cut off by a failed download still displays, and all that can follow the
// last marker is compressed data, the same as in a complete file.
func TestCutOffFilesAreCleaned(t *testing.T) {
	base := noisyJPEG(t)
	scanStart := len(base) - 2 - len(lastScan(t, base))
	metadata := segment(jpegmeta.MarkerCOM, []byte(hiddenText))
	withMeta := insertAfterSOI(base, metadata)
	offset := len(metadata)
	cases := map[string]struct {
		data []byte
		want []byte
	}{
		"no end marker":      {withMeta[:len(withMeta)-2], base},
		"half an end marker": {withMeta[:len(withMeta)-1], base},
		"inside the scan":    {withMeta[:offset+scanStart+100], slices.Concat(base[:scanStart+100], []byte{0xFF, jpegmeta.MarkerEOI})},
		"just after the header of the scan": {withMeta[:offset+scanStart],
			slices.Concat(base[:scanStart], []byte{0xFF, jpegmeta.MarkerEOI})},
		"inside a segment after the scan": {slices.Concat(withMeta[:len(withMeta)-2], []byte{0xFF, jpegmeta.MarkerCOM, 0x10, 0x00}, []byte(hiddenText)), base},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, tc.data, core.Options{})
			assertNoHiddenText(t, out)
			if !bytes.Equal(out, tc.want) {
				t.Fatalf("got %d bytes, want %d", len(out), len(tc.want))
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cut off") {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
			assertIdempotent(t, out)
		})
	}
	t.Run("before any scan there is no picture", func(t *testing.T) {
		refuses(t, withMeta[:offset+40], core.ErrMalformed)
	})
}

// arithmeticJPEG builds a file with an arithmetic-coded frame around the given
// tables. No standard library decoder reads one, so tests compare bytes; the review
// checked real decoding of such files against libjpeg-turbo.
func arithmeticJPEG(tables ...[]byte) []byte {
	out := grayHeader(0xC9)
	for _, table := range tables {
		out = append(out, table...)
	}
	out = append(out, sosSegment(0, 63)...)
	out = append(out, 0x12, 0x34, 0xFF, 0x00, 0x56)
	return append(out, 0xFF, jpegmeta.MarkerEOI)
}

func TestLosslessAndArithmeticFramesAreCopiedExactly(t *testing.T) {
	lossless := grayHeader(0xC3)
	lossless = append(lossless, dhtSegment(0, dcSymbols)...)
	lossless = append(lossless, segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x00, 1, 0, 0})...)
	lossless = append(lossless, 0x12, 0x34, 0xFF, 0x00, 0x56, 0xFF, jpegmeta.MarkerEOI)
	for name, data := range map[string][]byte{
		"lossless":             lossless,
		"arithmetic":           arithmeticJPEG(segment(jpegmeta.MarkerDAC, []byte{0x00, 0x10, 0x10, 5})),
		"arithmetic, no DAC":   arithmeticJPEG(),
		"arithmetic with DRI":  arithmeticJPEG(segment(jpegmeta.MarkerDRI, []byte{0, 1})),
		"progressive with DAC": bytes.Replace(arithmeticJPEG(segment(jpegmeta.MarkerDAC, []byte{0x00, 0x10})), []byte{0xFF, 0xC9}, []byte{0xFF, 0xCA}, 1),
	} {
		t.Run(name, func(t *testing.T) {
			metadata := segment(jpegmeta.MarkerCOM, []byte(hiddenText))
			out, rep := clean(t, insertAfterSOI(data, metadata), core.Options{})
			if !bytes.Equal(out, data) {
				t.Fatal("only the comment should have gone")
			}
			if !hasField(rep, "Comment", core.ActionRemove) {
				t.Fatalf("findings: %+v", rep.Findings)
			}
		})
	}
	t.Run("Huffman tables do nothing in an arithmetic frame", func(t *testing.T) {
		plain := arithmeticJPEG()
		data := arithmeticJPEG(dhtSegment(0, dcSymbols))
		out, rep := clean(t, data, core.Options{})
		if !bytes.Equal(out, plain) || !hasField(rep, "Unused decoding tables", core.ActionRemove) {
			t.Fatalf("findings: %+v", rep.Findings)
		}
	})
}

func TestScanHeaderTableIDsAreChecked(t *testing.T) {
	// A table id over 3 selects nothing a frame can define, and libjpeg refuses it.
	data := bytes.Replace(restartJPEG(false), segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x00, 0, 63, 0}), segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x70, 0, 63, 0}), 1)
	if bytes.Equal(data, restartJPEG(false)) {
		t.Fatal("the fixture did not change")
	}
	refuses(t, data, core.ErrMalformed)
	frame := bytes.Replace(restartJPEG(false), []byte{1, 0x11, 0, 0xFF, jpegmeta.MarkerDHT}, []byte{1, 0x11, 9, 0xFF, jpegmeta.MarkerDHT}, 1)
	if bytes.Equal(frame, restartJPEG(false)) {
		t.Fatal("the fixture did not change")
	}
	refuses(t, frame, core.ErrMalformed)
}

// A bad MakerNote offset is common, and viewers still read the orientation of such a
// block, so losing it would turn the picture sideways for good.
func TestOrientationSurvivesDamageElsewhereInTheBlock(t *testing.T) {
	base := noisyJPEG(t)
	root := coretest.FakeIFD(6)
	root.Exif.Entries = append(root.Exif.Entries, coretest.Entry{Tag: 0xA40B, Value: []byte("placeholder"), Count: 0x7FFFFFFF})
	data := insertAfterSOI(base, exifSegmentFor(root.Encode(binary.LittleEndian)))
	out, rep := clean(t, data, core.Options{})
	if !bytes.Contains(out, exif.Minimal(6)) {
		t.Fatal("the orientation was lost")
	}
	if !hasField(rep, "Orientation", core.ActionRetain) || !hasField(rep, "Damaged EXIF block", core.ActionRemove) {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	for _, secret := range coretest.FakeEXIFSecrets() {
		if bytes.Contains(out, secret) {
			t.Fatalf("%q survived", secret)
		}
	}
	strict, _ := clean(t, data, core.Options{Strict: true})
	if !bytes.Equal(strict, base) {
		t.Fatal("strict mode must remove it too")
	}
}

// Each table costs a record here for as little as 17 bytes of file, so a hostile file
// could otherwise spend several times its own size. Real files have dozens.
func TestTooManyTablesAreRefused(t *testing.T) {
	base := noisyJPEG(t)
	seg := segment(jpegmeta.MarkerDHT, bytes.Repeat(append([]byte{0x13}, make([]byte, 16)...), 3800))
	refuses(t, insertAfterSOI(base, slices.Repeat([][]byte{seg}, maxTableDefs/3800+1)...), core.ErrTooLarge)
}
