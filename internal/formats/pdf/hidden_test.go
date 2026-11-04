package pdf

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const hiddenSecret = "Test Hidden Secret"

// lzwEncode is the writer's half of LZWDecode, for fixtures.
func lzwEncode(data []byte, early int) []byte {
	var out []byte
	var acc uint32
	var nb uint
	put := func(code int, width uint) {
		acc = acc<<width | uint32(code)
		nb += width
		for nb >= 8 {
			nb -= 8
			out = append(out, byte(acc>>nb))
		}
		acc &= 1<<nb - 1
	}
	table := map[string]int{}
	reset := func() {
		clear(table)
		for i := range 256 {
			table[string([]byte{byte(i)})] = i
		}
	}
	reset()
	next, width := 258, uint(9)
	put(256, width)
	cur := ""
	grow := func() {
		next++
		if next+early-1 >= 1<<width && width < 12 {
			width++
		}
	}
	for _, c := range data {
		extended := cur + string([]byte{c})
		if _, ok := table[extended]; ok {
			cur = extended
			continue
		}
		put(table[cur], width)
		table[extended] = next
		grow()
		if next >= 4094 {
			put(256, width)
			reset()
			next, width = 258, 9
		}
		cur = string([]byte{c})
	}
	if cur != "" {
		put(table[cur], width)
		grow()
	}
	put(257, width)
	if nb > 0 {
		out = append(out, byte(acc<<(8-nb)))
	}
	return out
}

func runLengthEncode(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := min(len(data), 128)
		out = append(out, byte(n-1))
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, 128)
}

// pngSubAll applies the PNG "sub" filter, which leaves no run of plain bytes for a
// search to find, even in the first row.
func pngSubAll(data []byte, columns int) []byte {
	var out []byte
	for start := 0; start < len(data); start += columns {
		row := data[start:min(start+columns, len(data))]
		out = append(out, 1)
		for i, c := range row {
			if i == 0 {
				out = append(out, c)
			} else {
				out = append(out, c-row[i-1])
			}
		}
	}
	return out
}

func tiffEncode(data []byte, columns int) []byte {
	var out []byte
	for start := 0; start < len(data); start += columns {
		row := data[start:min(start+columns, len(data))]
		for i, c := range row {
			if i == 0 {
				out = append(out, c)
			} else {
				out = append(out, c-row[i-1])
			}
		}
	}
	return out
}

func a85Encode(data []byte) []byte {
	// Five digits for every four bytes, which is all the fixtures need: zero runs
	// get no z shorthand, and a final partial group follows the specification.
	var out []byte
	for len(data) > 0 {
		n := min(len(data), 4)
		var group [4]byte
		copy(group[:], data[:n])
		v := uint32(group[0])<<24 | uint32(group[1])<<16 | uint32(group[2])<<8 | uint32(group[3])
		var digits [5]byte
		for i := 4; i >= 0; i-- {
			digits[i] = byte(v%85) + '!'
			v /= 85
		}
		out = append(out, digits[:n+1]...)
		data = data[n:]
	}
	return append(out, "~>"...)
}

// privateStream refers to a stream from a place with no meaning, so nothing but
// its own bytes says what it is.
const privateStream = "/Group <</S /Transparency /Hidden 20 0 R>>"

func streamDoc(dict string, raw []byte) []byte {
	return doc{
		page:    privateStream,
		objects: func(b *builder) { b.stream(20, dict, raw) },
	}.build()
}

func contentDoc(dict string, raw []byte) []byte {
	return doc{contents: func(b *builder) { b.stream(4, dict, raw) }}.build()
}

// Item: XMP hidden behind each encoding the scan used to skip.
func TestXMPBehindEveryEncoding(t *testing.T) {
	packet := coretest.FakeXMP()
	cases := map[string][]byte{
		"LZW":           streamDoc("/Filter /LZWDecode", lzwEncode(packet, 1)),
		"LZW early 0":   streamDoc("/Filter /LZWDecode /DecodeParms <</EarlyChange 0>>", lzwEncode(packet, 0)),
		"abbreviated":   streamDoc("/Filter /LZW", lzwEncode(packet, 1)),
		"run length":    streamDoc("/Filter /RunLengthDecode", runLengthEncode(packet)),
		"PNG predictor": streamDoc("/Filter /FlateDecode /DecodeParms <</Predictor 11 /Columns 16>>", compress(pngSubAll(packet, 16))),
		"TIFF predictor": streamDoc("/Filter /FlateDecode /DecodeParms <</Predictor 2 /Columns 16>>",
			compress(tiffEncode(packet, 16))),
		"LZW and predictor": streamDoc("/Filter /LZWDecode /DecodeParms <</Predictor 11 /Columns 16>>",
			lzwEncode(pngSubAll(packet, 16), 1)),
		"predictor on an outer filter": streamDoc("/Filter [/FlateDecode /FlateDecode] /DecodeParms [<</Predictor 11 /Columns 16>> null]",
			compress(pngSubAll(compress(packet), 16))),
		"Crypt with the identity filter":       streamDoc("/Filter [/Crypt /FlateDecode]", compress(packet)),
		"filter under the inline abbreviation": streamDoc("/F /FlateDecode", compress(packet)),
		"chain under the inline abbreviation":  streamDoc("/F [/AHx /Fl]", hexEncode(compress(packet))),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			if got := valueOf(t, rep, "Creator"); got != "Test Author" {
				t.Errorf("Creator = %q", got)
			}
			if bytes.Contains(out, []byte("/Hidden")) {
				t.Errorf("the stream survived:\n%s", out)
			}
		})
	}
}

// Item: streams that are kept because removing them would damage the document must
// say so when they hold a packet.
func TestXMPInKeptStreamsWarns(t *testing.T) {
	packet := compress(coretest.FakeXMP())
	flate := "/Filter /FlateDecode "
	cases := map[string][]byte{
		"page content": doc{page: "/Contents [4 0 R 20 0 R]", objects: func(b *builder) {
			b.stream(20, flate, packet)
		}}.build(),
		"form XObject": doc{page: "/Resources <</XObject <</Fm0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, flate+"/Subtype /Form /BBox [0 0 1 1]", packet)
		}}.build(),
		"tiling pattern": doc{page: "/Resources <</Pattern <</P0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, flate+"/PatternType 1 /BBox [0 0 1 1]", packet)
		}}.build(),
		"a stream that only claims a BBox": streamDoc(flate+"/BBox [0 0 1 1]", packet),
		"font program":                     streamDoc(flate+"/Length1 3", packet),
		"compact font":                     streamDoc(flate+"/Subtype /Type1C", packet),
		"image": doc{page: "/Resources <</XObject <</Im0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, flate+"/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", packet)
		}}.build(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			wantWarning(t, rep, "XMP packet")
			if !bytes.Contains(out, packet) {
				t.Errorf("the stream was changed:\n%s", out)
			}
			if residual := inspect(t, out).Residual(); len(residual) != 0 {
				t.Errorf("residual findings: %+v", residual)
			}
		})
	}
}

// Item: inline images.
func TestInlineImagesWarn(t *testing.T) {
	jpeg := fakeJPEG(true)
	withImage := func(image string) []byte {
		return []byte(pageBody + "\nq\nBI " + image + " ID " + string(jpeg) + " EI\nQ\n")
	}
	wants := map[string][]byte{
		"JPEG": contentDoc("", withImage("/W 1 /H 1 /BPC 8 /CS /RGB /F /DCT")),
		"JPEG in compressed content": contentDoc("/Filter /FlateDecode",
			compress(withImage("/W 1 /H 1 /BPC 8 /CS /RGB /F /DCT"))),
		"full names":  contentDoc("/Filter /FlateDecode", compress(withImage("/Width 1 /Height 1 /Filter /DCTDecode"))),
		"in a chain":  contentDoc("/Filter /FlateDecode", compress(withImage("/W 1 /H 1 /F [/A85 /DCT]"))),
		"JPEG 2000":   contentDoc("/Filter /FlateDecode", compress(withImage("/W 1 /H 1 /F /JPXDecode"))),
		"private key": contentDoc("/Filter /FlateDecode", compress(withImage("/W 1 /H 1 /BPC 8 /CS /G /Author ("+hiddenSecret+")"))),
		"no space before the name": contentDoc("/Filter /FlateDecode",
			compress([]byte("q BI/W 1/H 1/F/DCT ID "+string(jpeg)+" EI Q"))),
		"form XObject": doc{page: "/Resources <</XObject <</Fm0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, "/Subtype /Form /BBox [0 0 1 1] /Filter /FlateDecode", compress(withImage("/W 1 /H 1 /F /DCT")))
		}}.build(),
		"Type 3 glyph": doc{page: "/Resources <</Font <</T3 20 0 R>>>>", objects: func(b *builder) {
			b.obj(20, "<</Type /Font /Subtype /Type3 /CharProcs <</a 21 0 R>>>>")
			b.stream(21, "/Filter /FlateDecode", compress(withImage("/W 1 /H 1 /F /DCT")))
		}}.build(),
		"past the first read": contentDoc("/Filter /FlateDecode",
			compress(append(bytes.Repeat([]byte("q Q "), 64<<10), withImage("/W 1 /H 1 /F /DCT")...))),
		"an oversized dictionary": contentDoc("/Filter /FlateDecode",
			compress(withImage("/W 1 /H 1 /Note ("+strings.Repeat("x", 8192)+")"))),
	}
	for name, data := range wants {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			wantWarning(t, rep, "inline")
			for _, w := range rep.Warnings {
				if strings.Contains(w, hiddenSecret) {
					t.Errorf("warning %q quotes the value", w)
				}
			}
		})
	}
	nothing := map[string][]byte{
		"plain bitmap":      contentDoc("/Filter /FlateDecode", compress([]byte("q BI /W 1 /H 1 /BPC 1 /IM true /D [1 0] ID \x7f EI Q"))),
		"fax-coded glyph":   contentDoc("", []byte("q BI /W 1 /H 1 /BPC 1 /IM true /F /CCF /DP <</K -1>> ID \x00 EI Q")),
		"text that says BI": contentDoc("/Filter /FlateDecode", compress([]byte("BT (BI /W 1 ID EI) Tj ET"))),
		"a word ending BI":  contentDoc("/Filter /FlateDecode", compress([]byte("/ABI /F /DCT ID"))),
		"not a dictionary":  contentDoc("/Filter /FlateDecode", compress([]byte("q BI 12 ID Q"))),
	}
	for name, data := range nothing {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			if len(rep.Warnings) != 0 {
				t.Errorf("unexpected warnings: %q", rep.Warnings)
			}
		})
	}
}

// A tiny stream of unterminated candidates must not make each one read to the end
// of its window, which would be quadratic in anything but name.
func TestInlineScanIsBounded(t *testing.T) {
	coretest.SkipIfRace(t)
	data := contentDoc("/Filter /FlateDecode", compress(bytes.Repeat([]byte("BI /A("), 16<<20)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the inline image scan did not finish in 30 seconds")
	}
}

func TestContentCommentsWarn(t *testing.T) {
	_, rep := clean(t, contentDoc("/Filter /FlateDecode", compress([]byte(pageBody+"\n% "+hiddenSecret+"\n"))), core.Options{})
	wantWarning(t, rep, "comments")
	for _, w := range rep.Warnings {
		if strings.Contains(w, hiddenSecret) {
			t.Errorf("warning %q quotes the value", w)
		}
	}
	for name, body := range map[string]string{
		"percent inside a string": "BT (100% sure) Tj ET",
		"escaped parenthesis":     `BT (a\) % b) Tj ET`,
		"nested parentheses":      "BT (a(b)% c) Tj ET",
		"an empty comment":        "q %\nQ",
		"inline image bytes":      "q BI /W 1 /H 1 /BPC 8 /CS /G ID %secret EI Q",
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, contentDoc("/Filter /FlateDecode", compress([]byte(body))), core.Options{})
			if len(rep.Warnings) != 0 {
				t.Errorf("unexpected warnings: %q", rep.Warnings)
			}
		})
	}
	// A comment in a CMap or a function is how those are written.
	_, rep = clean(t, doc{page: "/Resources <</Font <</F1 20 0 R>>>>", objects: func(b *builder) {
		b.obj(20, "<</Type /Font /Subtype /Type0 /ToUnicode 21 0 R>>")
		b.stream(21, "/Filter /FlateDecode", compress([]byte("%!PS-Adobe-3.0 Resource-CMap\n")))
	}}.build(), core.Options{})
	if len(rep.Warnings) != 0 {
		t.Errorf("a CMap's comment warned: %q", rep.Warnings)
	}
}

// Item: private keys in fonts and actions.
func TestPrivateKeysInFontsAndActions(t *testing.T) {
	const private = "/VendorNote (" + hiddenSecret + ") "
	cases := map[string]struct {
		data  []byte
		field string
		keep  []string
	}{
		"action on the catalog": {
			doc{catalog: "/OpenAction <</S /GoTo /D [3 0 R /Fit] " + private + ">>"}.build(),
			"Unknown action entry", []string{"/S /GoTo", "/D [3 0 R /Fit]"},
		},
		"action with a type and no standard name": {
			doc{catalog: "/OpenAction <</Type /Action /S /Vendor " + private + "/Next <</S /Named /N /NextPage " + private + ">>>>"}.build(),
			"Unknown action entry", []string{"/N /NextPage"},
		},
		"indirect action on a link": {
			doc{page: "/Annots [<</Subtype /Link /Rect [0 0 1 1] /A 20 0 R>>]", objects: func(b *builder) {
				b.obj(20, "<</S /URI /URI (http://example.invalid/) "+private+">>")
			}}.build(),
			"Unknown action entry", []string{"/URI (http://example.invalid/)"},
		},
		"font reached by reference": {
			doc{page: "/Resources <</Font <</F2 20 0 R>>>>", objects: func(b *builder) {
				b.obj(20, "<</Type /Font /Subtype /TrueType /BaseFont /TestFont /FirstChar 32 /LastChar 32 /Widths [1] "+
					"/FontDescriptor 21 0 R "+private+">>")
				b.obj(21, "<</Type /FontDescriptor /FontName /TestFont /Flags 32 /FontBBox [0 0 1 1] /ItalicAngle 0 "+
					"/Ascent 1 /Descent 0 /CapHeight 1 /StemV 1 "+private+">>")
			}}.build(),
			"Unknown font entry", []string{"/BaseFont /TestFont", "/Widths [1]", "/FontDescriptor", "/FontName /TestFont", "/StemV 1"},
		},
		"font with no /Type": {
			doc{page: "/Resources <</Font <</F2 <</Subtype /Type1 /BaseFont /Helvetica " + private + ">>>>>>"}.build(),
			"Unknown font entry", []string{"/BaseFont /Helvetica"},
		},
		"composite font": {
			doc{page: "/Resources <</Font <</F2 20 0 R>>>>", objects: func(b *builder) {
				b.obj(20, "<</Type /Font /Subtype /Type0 /BaseFont /TestFont /Encoding /Identity-H /DescendantFonts [21 0 R] "+private+">>")
				b.obj(21, "<</Type /Font /Subtype /CIDFontType2 /BaseFont /TestFont /CIDSystemInfo <</Registry (Adobe) /Ordering (Identity) /Supplement 0>> "+
					"/DW 1000 /W [0 [1]] /CIDToGIDMap /Identity "+private+">>")
			}}.build(),
			"Unknown font entry", []string{"/DescendantFonts", "/CIDSystemInfo", "/DW 1000", "/W [0 [1]]", "/CIDToGIDMap /Identity"},
		},
		"private data reached only through a private key": {
			doc{catalog: "/OpenAction <</S /GoTo /D [3 0 R /Fit] /Vendor 20 0 R>>", objects: func(b *builder) {
				b.obj(20, "<</Who ("+hiddenSecret+")>>")
			}}.build(),
			"Unknown action entry", nil,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, c.data, core.Options{})
			if bytes.Contains(out, []byte(hiddenSecret)) {
				t.Errorf("the private value survived:\n%s", out)
			}
			if len(valuesOf(rep, c.field)) == 0 {
				t.Errorf("no finding %q in %+v", c.field, rep.Findings)
			}
			for _, want := range c.keep {
				if !bytes.Contains(out, []byte(want)) {
					t.Errorf("the output lost %q:\n%s", want, out)
				}
			}
			if !bytes.Equal(cleanOK(t, out), out) {
				t.Error("not idempotent")
			}
		})
	}
	// A structure element also has /S, and must not be taken for an action.
	out := cleanOK(t, doc{catalog: "/StructTreeRoot <</Type /StructTreeRoot /K [20 0 R]>>", objects: func(b *builder) {
		b.obj(20, "<</S /URI /P 1 0 R /K [0] /Pg 3 0 R>>")
	}}.build())
	if !bytes.Contains(out, []byte("/K [0]")) {
		t.Errorf("a structure element was treated as an action:\n%s", out)
	}
}

// Everywhere there is no allowlist, a key HideMe has never heard of cannot be
// removed safely, but it can be noticed.
func TestUnrecognisedKeysWarn(t *testing.T) {
	const private = "/VendorNote (" + hiddenSecret + ")"
	cases := map[string][]byte{
		"annotation":        doc{page: "/Annots [<</Subtype /Link /Rect [0 0 1 1] " + private + ">>]"}.build(),
		"graphics state":    doc{page: "/Resources <</ExtGState <</GS0 <</Type /ExtGState /CA 1 " + private + ">>>>>>"}.build(),
		"structure element": doc{catalog: "/StructTreeRoot <</Type /StructTreeRoot /K [<</S /P " + private + ">>]>>"}.build(),
		"indirect layer":    doc{catalog: "/OCProperties <</OCGs [20 0 R] /D <<>>>>", objects: func(b *builder) { b.obj(20, "<</Type /OCG /Name (x) "+private+">>") }}.build(),
		"font stream": doc{page: "/Resources <</Font <</F2 20 0 R>>>>", objects: func(b *builder) {
			b.obj(20, "<</Type /FontDescriptor /FontName /X /Flags 1 /FontFile2 21 0 R>>")
			b.stream(21, "/Length1 1 "+private, []byte{0})
		}}.build(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			wantWarning(t, rep, "does not recognise")
			if got := valuesOf(rep, "Unrecognised key"); len(got) == 0 || !strings.Contains(got[0], hiddenSecret) {
				t.Errorf("Unrecognised key = %q", got)
			}
			for _, w := range rep.Warnings {
				if strings.Contains(w, hiddenSecret) {
					t.Errorf("warning %q quotes the value", w)
				}
			}
		})
	}
	// Maps of names to resources hold names their producer chose, and those are not
	// private keys.
	out, rep := clean(t, doc{
		catalog: "/Dests <</VendorDest [3 0 R /Fit]>> /StructTreeRoot <</RoleMap <</VendorTag /P>> /ClassMap <</VendorClass <<>>>>>>",
		page: "/Annots [<</Subtype /Widget /Rect [0 0 1 1] /FT /Btn /T (x) /AP <</N <</VendorState 20 0 R>>>>>>] " +
			"/Resources <</Font <</VendorFont 5 0 R>> /ExtGState <</VendorGS <<>>>> /ColorSpace <</VendorCS /DeviceRGB>> /Properties <</VendorP <<>>>>>>",
		objects: func(b *builder) { b.stream(20, "/Subtype /Form /BBox [0 0 1 1]", []byte("q Q")) },
	}.build(), core.Options{})
	if got := valuesOf(rep, "Unrecognised key"); len(got) != 0 {
		t.Errorf("a name in a map was called unrecognised: %q", got)
	}
	if !bytes.Contains(out, []byte("/VendorFont")) {
		t.Errorf("the output lost a resource:\n%s", out)
	}
	// Resources themselves have keys the specification enumerates.
	out, rep = clean(t, doc{page: "/Resources <</Font <</F1 5 0 R>> /Vendor <</Who (" + hiddenSecret + ")>>>>"}.build(), core.Options{})
	if bytes.Contains(out, []byte(hiddenSecret)) {
		t.Errorf("a private resource category survived:\n%s", out)
	}
	if len(valuesOf(rep, "Unknown resource entry")) == 0 {
		t.Errorf("no finding in %+v", rep.Findings)
	}
}

// A dictionary used as a map of names may also hide behind that role: a page that
// also serves as its own font map must still get the page's rules.
func TestNameMapRoleDoesNotExemptAPage(t *testing.T) {
	data := doc{
		page: "/Resources <</Font 3 0 R>> /VendorNote (" + hiddenSecret + ") /PieceInfo <</A <</B (" + hiddenSecret + ")>>>>",
	}.build()
	if out := cleanOK(t, data); bytes.Contains(out, []byte(hiddenSecret)) {
		t.Errorf("a page escaped its rules by being used as a resource map:\n%s", out)
	}
}

// Names in a map are the producer's choice, and the removal rules are about keys.
func TestResourceNamesAreNotRemovalRules(t *testing.T) {
	for _, name := range []string{"EF", "Thumb", "Metadata", "AF", "OPI", "DPM", "PieceInfo", "LastModified", "CreatorInfo", "AA", "User"} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, doc{
				page: fmt.Sprintf("/Resources <</Font <</F1 5 0 R /%s 5 0 R>> /XObject <</%s 20 0 R>>>>", name, name),
				objects: func(b *builder) {
					b.stream(20, "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray", []byte{0x7F})
				},
			}.build(), core.Options{})
			for _, want := range []string{"/F1 ", "/" + name + " ", "/Subtype /Image"} {
				if !bytes.Contains(out, []byte(want)) {
					t.Errorf("the output lost %q:\n%s", want, out)
				}
			}
			if len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
				t.Errorf("a resource's name was reported: %+v %q", rep.Findings, rep.Warnings)
			}
		})
	}
	out := cleanOK(t, doc{catalog: "/Dests <</Metadata [3 0 R /Fit] /Thumb 20 0 R>>", objects: func(b *builder) {
		b.obj(20, "<</D [3 0 R /Fit]>>")
	}}.build())
	if !bytes.Contains(out, []byte("/Metadata [")) || !bytes.Contains(out, []byte("/Thumb ")) {
		t.Errorf("a named destination was lost:\n%s", out)
	}
	// What a name leads to is still judged on its own.
	out, rep := clean(t, doc{page: "/Resources <</XObject <</Metadata 20 0 R>>>>", objects: func(b *builder) {
		b.stream(20, "/Type /Metadata /Subtype /XML", coretest.FakeXMP())
	}}.build(), core.Options{})
	if bytes.Contains(out, []byte("x:xmpmeta")) || len(valuesOf(rep, "Creator")) == 0 {
		t.Errorf("a metadata stream got past a map:\n%s", out)
	}
}

// Item: bytes after the end of filtered data.
func TestBytesAfterStreamEnd(t *testing.T) {
	body := []byte(pageBody)
	secret := []byte(hiddenSecret)
	cases := map[string]struct {
		dict string
		raw  []byte
	}{
		"flate":             {"/Filter /FlateDecode", slices.Concat(compress(body), secret)},
		"bare deflate":      {"/Filter /FlateDecode", slices.Concat(compress(body)[2:len(compress(body))-4], secret)},
		"flate in an array": {"/Filter [/FlateDecode]", slices.Concat(compress(body), secret)},
		"flate and predictor": {"/Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 8>>",
			slices.Concat(compress(pngSubAll(body, 8)), secret)},
		"hex":        {"/Filter /ASCIIHexDecode", slices.Concat(hexEncode(body), secret)},
		"ascii85":    {"/Filter /ASCII85Decode", slices.Concat(a85Encode(body), secret)},
		"LZW":        {"/Filter /LZWDecode", slices.Concat(lzwEncode(body, 1), secret)},
		"run length": {"/Filter /RunLengthDecode", slices.Concat(runLengthEncode(body), secret)},
		"outside a chain": {"/Filter [/ASCIIHexDecode /FlateDecode]",
			slices.Concat(hexEncode(compress(body)), secret)},
		"inside hex": {"/Filter [/ASCIIHexDecode /FlateDecode]",
			hexEncode(slices.Concat(compress(body), secret))},
		"inside flate": {"/Filter [/FlateDecode /FlateDecode]",
			compress(slices.Concat(compress(body), secret))},
		"inside a predictor": {"/Filter [/FlateDecode /FlateDecode] /DecodeParms [<</Predictor 11 /Columns 8>> null]",
			compress(pngSubAll(slices.Concat(compress(body), secret), 8))},
		"at both levels": {"/Filter [/ASCII85Decode /LZWDecode]",
			slices.Concat(a85Encode(slices.Concat(lzwEncode(body, 1), secret)), secret)},
		"inside the abbreviation": {"/F [/AHx /Fl]",
			hexEncode(slices.Concat(compress(body), secret))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			data := contentDoc(c.dict, c.raw)
			out, rep := clean(t, data, core.Options{})
			if got := pagesContent(t, out); len(got) != 1 || !bytes.Equal(got[0], body) {
				t.Fatalf("the page changed to %q", got)
			}
			if bytes.Contains(rawLayersOf(t, out), secret) {
				t.Errorf("the hidden bytes survived:\n%q", out)
			}
			if got := valuesOf(rep, "Data after a stream's end"); len(got) == 0 {
				t.Errorf("not reported: %+v", rep.Findings)
			}
			if residual := inspect(t, out).Residual(); len(residual) != 0 {
				t.Errorf("residual findings: %+v", residual)
			}
			if !bytes.Equal(cleanOK(t, out), out) {
				t.Error("not idempotent")
			}
			if !bytes.Equal(cleanOK(t, data), out) {
				t.Error("not deterministic")
			}
		})
	}
}

// rawLayersOf returns a file followed by every stream in it decoded as far as it
// goes, and the stages in between, so that nothing is hidden by being encoded.
func rawLayersOf(t *testing.T, data []byte) []byte {
	t.Helper()
	d, err := load(context.Background(), data, defaultLimits)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out := bytes.Clone(data)
	for num, e := range d.entries {
		val, _, err := d.lookup(pdfRef{num, e.gen})
		if err != nil {
			continue
		}
		s, ok := val.(*pdfStream)
		if !ok {
			continue
		}
		steps, err := d.filterChain(s.dict)
		if err != nil {
			continue
		}
		for i := 1; i <= len(steps); i++ {
			if layer, err := d.decodeSteps(s.raw, steps[:i]); err == nil {
				out = append(out, layer...)
			}
		}
	}
	return out
}

// Writers' habits around the end of compressed data are not findings, and a stream
// that is already exact must not change by one byte.
func TestStreamEndsThatAreFine(t *testing.T) {
	body := []byte(pageBody)
	z := compress(body)
	for name, c := range map[string]struct {
		dict string
		raw  []byte
		want []byte
	}{
		"exact flate":                {"/Filter /FlateDecode", z, z},
		"exact hex":                  {"/Filter /ASCIIHexDecode", hexEncode(body), hexEncode(body)},
		"exact ascii85":              {"/Filter /ASCII85Decode", a85Encode(body), a85Encode(body)},
		"exact LZW":                  {"/Filter /LZWDecode", lzwEncode(body, 1), lzwEncode(body, 1)},
		"exact run length":           {"/Filter /RunLengthDecode", runLengthEncode(body), runLengthEncode(body)},
		"no checksum":                {"/Filter /FlateDecode", z[:len(z)-4], z[:len(z)-4]},
		"a line end counted":         {"/Filter /FlateDecode", slices.Concat(z, []byte("\r\n")), z},
		"padding counted":            {"/Filter /FlateDecode", slices.Concat(z, make([]byte, 16)), z},
		"a line end in hex":          {"/Filter /ASCIIHexDecode", slices.Concat(hexEncode(body), []byte("\n")), hexEncode(body)},
		"a line end after ascii85":   {"/Filter /ASCII85Decode", slices.Concat(a85Encode(body), []byte("\n")), a85Encode(body)},
		"a line end inside a chain":  {"/Filter [/ASCIIHexDecode /FlateDecode]", hexEncode(slices.Concat(z, []byte("\n"))), hexEncode(slices.Concat(z, []byte("\n")))},
		"ascii85 with no end marker": {"/Filter /ASCII85Decode", bytes.TrimSuffix(a85Encode(body), []byte("~>")), bytes.TrimSuffix(a85Encode(body), []byte("~>"))},
		"truncated flate":            {"/Filter /FlateDecode", z[:len(z)/2], z[:len(z)/2]},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, contentDoc(c.dict, c.raw), core.Options{})
			if !bytes.Contains(out, slices.Concat([]byte("stream\n"), c.want, []byte("\nendstream"))) {
				t.Errorf("stream data changed:\n%q", out)
			}
			if len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
				t.Errorf("reported something: %+v %q", rep.Findings, rep.Warnings)
			}
		})
	}
}

// If a filter's data is damaged, where it should have ended is unknowable. A byte
// search would still find what follows, so the file must say it was not checked.
func TestDamagedStreamsWarn(t *testing.T) {
	for name, c := range map[string]struct {
		dict string
		raw  []byte
	}{
		"not deflate at all":     {"/Filter /FlateDecode", []byte(hiddenSecret)},
		"bad hex":                {"/Filter /ASCIIHexDecode", []byte("zz" + hiddenSecret)},
		"ascii85 with a bad end": {"/Filter /ASCII85Decode", []byte("87cUR~" + hiddenSecret)},
		"bad LZW code":           {"/Filter /LZWDecode", []byte{0xFF, 0xFF, 0xFF, 0xFF}},
		"bad predictor tag":      {"/Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 1>>", compress([]byte{9, 1})},
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, streamDoc(c.dict, c.raw), core.Options{})
			wantWarning(t, rep, "could not be fully decoded")
		})
	}
}

// Item: other filters whose data cannot be inspected.
func TestOpaqueFiltersWarn(t *testing.T) {
	image := "/Subtype /Image /Width 8 /Height 1 /BitsPerComponent 1 /ImageMask true "
	for name, dict := range map[string]string{
		"fax":                 image + "/Filter /CCITTFaxDecode",
		"JBIG2":               image + "/Filter /JBIG2Decode",
		"JPEG 2000":           image + "/Filter /JPXDecode",
		"after a wrapper":     image + "/Filter [/ASCIIHexDecode /JBIG2Decode]",
		"unknown":             "/Filter /VendorDecode",
		"encrypting Crypt":    "/Filter /Crypt /DecodeParms <</Name /Vendor>>",
		"bad predictor":       "/Filter /FlateDecode /DecodeParms <</Predictor 3>>",
		"deep TIFF predictor": "/Filter /FlateDecode /DecodeParms <</Predictor 2 /BitsPerComponent 16 /Columns 2>>",
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, streamDoc(dict, []byte("Test Opaque Bytes>")), core.Options{})
			wantWarning(t, rep, "cannot look inside")
			if !bytes.Contains(out, []byte("Test Opaque Bytes")) {
				t.Errorf("the data must be kept:\n%s", out)
			}
		})
	}
	// The wrapper around an opaque filter can still be checked.
	out, rep := clean(t, streamDoc("/Filter [/ASCIIHexDecode /JBIG2Decode]", []byte("00>"+hiddenSecret)), core.Options{})
	wantWarning(t, rep, "cannot look inside")
	if bytes.Contains(out, []byte(hiddenSecret)) {
		t.Errorf("bytes after the wrapper's end survived:\n%s", out)
	}
}

// Item: a JPEG in a wrapper this package used not to decode.
func TestJPEGInEveryWrapper(t *testing.T) {
	image := "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceRGB "
	for name, c := range map[string]struct {
		dict string
		raw  []byte
	}{
		"LZW":               {"/Filter [/LZWDecode /DCTDecode]", lzwEncode(fakeJPEG(true), 1)},
		"LZW and flate":     {"/Filter [/FlateDecode /LZWDecode /DCTDecode]", compress(lzwEncode(fakeJPEG(true), 1))},
		"run length":        {"/Filter [/RunLengthDecode /DCTDecode]", runLengthEncode(fakeJPEG(true))},
		"predictor":         {"/Filter [/FlateDecode /DCTDecode] /DecodeParms [<</Predictor 11 /Columns 8>> null]", compress(pngSubAll(fakeJPEG(true), 8))},
		"abbreviated":       {"/F /DCT", fakeJPEG(true)},
		"abbreviated chain": {"/F [/AHx /DCT] /DP [null <</ColorTransform 1>>]", hexEncode(fakeJPEG(true))},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, doc{page: "/Resources <</XObject <</Im0 20 0 R>>>>", objects: func(b *builder) {
				b.stream(20, image+c.dict, c.raw)
			}}.build(), core.Options{})
			if got := valueOf(t, rep, "GPS coordinates"); got != coretest.EiffelTower {
				t.Errorf("GPS coordinates = %q", got)
			}
			if !bytes.Contains(out, fakeJPEG(false)) {
				t.Errorf("the cleaned, unwrapped image is missing:\n%q", out)
			}
			if bytes.Contains(rawLayersOf(t, out), []byte("Test Camera GPS")) {
				t.Errorf("the metadata survived:\n%q", out)
			}
			if !bytes.Contains(out, []byte("/Filter /DCTDecode")) || bytes.Contains(out, []byte("/F /")) || bytes.Contains(out, []byte("/F [")) {
				t.Errorf("the dictionary does not describe the data:\n%q", out)
			}
			if !bytes.Equal(cleanOK(t, out), out) {
				t.Error("not idempotent")
			}
		})
	}
}

// /F on a stream normally names an external file. As a name it is a filter, which
// one reader may honour and another may not, so two chains cannot be reconciled.
func TestFilterUnderTwoNames(t *testing.T) {
	data := streamDoc("/Filter /ASCIIHexDecode /F /FlateDecode", []byte("00>"))
	if _, err := testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestFilterChainLimit(t *testing.T) {
	data := streamDoc("/Filter ["+strings.Repeat("/Fl ", 100)+"]", []byte("x"))
	if _, err := testHandler.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

// A predictor's row size is a claim, and a tiny stream must not be able to spend
// that claim's worth of memory.
func TestPredictorClaimsCostNoMemory(t *testing.T) {
	d := newDocument(context.Background(), nil, defaultLimits)
	p := parser{data: []byte("<</Filter /FlateDecode /DecodeParms <</Predictor 12 /Colors 32 /BitsPerComponent 16 /Columns 1000000>>>>"), budget: &valueBudget{limit: 100}}
	v, err := p.value(0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.decodeStream(&pdfStream{dict: v.(*pdfDict), raw: compress([]byte{0, 'a', 'b'})})
	if err != nil || string(got) != "ab" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestLZWDecoder(t *testing.T) {
	long := make([]byte, 200<<10)
	for i := range long {
		long[i] = byte(i * i >> 3) // enough variety to walk every code width and overflow the table
	}
	cases := map[string][]byte{
		"empty":    {},
		"one":      {'a'},
		"repeats":  bytes.Repeat([]byte("a"), 5000), // the code that is defined by its own use
		"pattern":  bytes.Repeat([]byte("abcab"), 1000),
		"long":     long,
		"high":     bytes.Repeat([]byte{0xFF, 0x00, 0x80}, 1000),
		"specials": {0xFF, 0x00, 0x01, 0x00, 0x01, 0x01},
	}
	for name, plain := range cases {
		for _, early := range []int{1, 0} {
			t.Run(fmt.Sprintf("%s early %d", name, early), func(t *testing.T) {
				dict := &pdfDict{}
				dict.set("Filter", pdfName("LZWDecode"))
				parms := &pdfDict{}
				parms.set("EarlyChange", int64(early))
				dict.set("DecodeParms", parms)
				d := newDocument(context.Background(), nil, defaultLimits)
				encoded := lzwEncode(plain, early)
				got, err := d.decodeStream(&pdfStream{dict: dict, raw: encoded})
				if len(plain) == 0 {
					if err == nil && len(got) != 0 {
						t.Errorf("got %d bytes from nothing", len(got))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("got %d bytes, want %d; first difference near %d", len(got), len(plain), mismatch(got, plain))
				}
			})
		}
	}
}

func mismatch(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// Run length and LZW bombs reach the same caps as flate.
func TestNewFiltersAreCapped(t *testing.T) {
	small := defaultLimits
	small.maxScanned = 1 << 16
	h := handler{limits: small}
	bomb := bytes.Repeat([]byte{129, 'x'}, 4096) // 128 bytes each
	data := streamDoc("/Filter /RunLengthDecode", bomb)
	if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestExcessImageDataWarns(t *testing.T) {
	image := func(extra string) string {
		return "/Subtype /Image /Width 2 /Height 2 /BitsPerComponent 8 " + extra
	}
	for name, c := range map[string]struct {
		dict string
		raw  []byte
		warn bool
	}{
		"gray, exact":             {image("/ColorSpace /DeviceGray"), []byte{1, 2, 3, 4}, false},
		"gray, extra":             {image("/ColorSpace /DeviceGray"), []byte("abcd" + hiddenSecret), true},
		"gray, flate, extra":      {image("/ColorSpace /DeviceGray /Filter /FlateDecode"), compress([]byte("abcd" + hiddenSecret)), true},
		"gray, line end only":     {image("/ColorSpace /DeviceGray /Filter /FlateDecode"), compress([]byte("abcd\n")), false},
		"RGB, exact":              {image("/ColorSpace /DeviceRGB"), make([]byte, 12), false},
		"RGB, extra":              {image("/ColorSpace /DeviceRGB"), []byte("123456789012" + hiddenSecret), true},
		"indexed, extra":          {image("/ColorSpace [/Indexed /DeviceRGB 1 <000000FFFFFF>]"), []byte("abcd" + hiddenSecret), true},
		"ICC, extra":              {image("/ColorSpace [/ICCBased 21 0 R]"), []byte("123456789012" + hiddenSecret), true},
		"mask, extra":             {"/Subtype /Image /Width 8 /Height 1 /ImageMask true", []byte("a" + hiddenSecret), true},
		"mask, exact":             {"/Subtype /Image /Width 8 /Height 1 /ImageMask true", []byte{1}, false},
		"abbreviated keys, extra": {"/Subtype /Image /W 1 /H 1 /BPC 8 /CS /G", []byte("a" + hiddenSecret), true},
		"unknown colour space":    {image("/ColorSpace /Vendor"), []byte("abcd" + hiddenSecret), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, doc{page: "/Resources <</XObject <</Im0 20 0 R>>>>", objects: func(b *builder) {
				b.stream(20, c.dict, c.raw)
				b.stream(21, "/N 3", []byte{0})
			}}.build(), core.Options{})
			has := slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, "more data than") })
			if has != c.warn {
				t.Errorf("warned = %v, want %v: %q", has, c.warn, rep.Warnings)
			}
		})
	}
}

// Item: everything outside an object is gone, and an object stream's leftovers with it.
func TestRewriteDropsWhatIsBetweenObjects(t *testing.T) {
	b := newBuilder("1.7 " + hiddenSecret)
	b.buf.WriteString("% " + hiddenSecret + " after the header\n")
	b.obj(1, "<</Type /Catalog /Pages 2 0 R % "+hiddenSecret+" in a dictionary\n>>")
	b.buf.WriteString(hiddenSecret + " between objects\n% " + hiddenSecret + " in a comment between\n")
	b.obj(2, "% "+hiddenSecret+" at the start\n<</Type /Pages /Kids [3 0 R % "+hiddenSecret+" in an array\n] /Count 1>>")
	b.obj(3, "<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R>>")
	b.offsets[4] = b.buf.Len()
	fmt.Fprintf(&b.buf, "4 0 obj %% %s\n<</Length %d>>\nstream\n%s\nendstream %s\n%s endobj %s\n",
		hiddenSecret, len(pageBody), pageBody, hiddenSecret, hiddenSecret, hiddenSecret)
	b.table("/Root 1 0 R")
	if out := cleanOK(t, b.bytes()); bytes.Contains(out, []byte(hiddenSecret)) {
		t.Errorf("a comment or stray bytes survived:\n%s", out)
	}

	// An object stream, with comments in its directory and between its objects, an
	// unlisted object at the end, and bytes after its compressed data.
	o := newBuilder("1.5")
	rows := make(map[int][3]int)
	o.stream(4, "", []byte(pageBody))
	rows[4] = [3]int{1, o.offsets[4], 0}
	gap := " % " + hiddenSecret + " in the gap\n"
	bodies := []string{
		"<</Type /Catalog /Pages 2 0 R>>",
		"<</Type /Pages /Kids [3 0 R] /Count 1>>",
		"<</Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R>>",
	}
	var head, body strings.Builder
	for i, text := range bodies {
		fmt.Fprintf(&head, "%d %d ", i+1, body.Len())
		body.WriteString(text + gap)
		rows[i+1] = [3]int{2, 6, i}
	}
	body.WriteString("<</Author (" + hiddenSecret + ")>> unlisted " + hiddenSecret)
	head.WriteString("% " + hiddenSecret + " in the directory\n")
	packed := slices.Concat(compress([]byte(head.String()+body.String())), []byte(hiddenSecret))
	o.stream(6, fmt.Sprintf("/Type /ObjStm /N 3 /First %d /Filter /FlateDecode /Note (%s)", head.Len(), hiddenSecret), packed)
	rows[6] = [3]int{1, o.offsets[6], 0}
	at := o.xrefStream(7, 8, rows, "/Root 1 0 R")
	fmt.Fprintf(&o.buf, "startxref\n%d\n%%%%EOF\n", at)
	out := cleanOK(t, o.bytes())
	if bytes.Contains(out, []byte(hiddenSecret)) {
		t.Errorf("an object stream's leftovers survived:\n%s", out)
	}
	if got := pagesContent(t, out); len(got) != 1 || !bytes.Equal(got[0], []byte(pageBody)) {
		t.Errorf("the page became %q", got)
	}
}

// Paths outside the keys that used to be checked.
func TestEveryPathIsReported(t *testing.T) {
	image := "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray "
	for name, data := range map[string][]byte{
		"Launch with Windows parameters": doc{catalog: `/OpenAction <</S /Launch /Win <</F (C:\\Users\\Test User\\x.exe)>>>>`}.build(),
		"Launch with Mac parameters":     doc{catalog: `/OpenAction <</S /Launch /Mac (/Users/Test User/x)>>`}.build(),
		"a stream's external file": doc{page: "/Resources <</XObject <</Im0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, image+"/F (/home/Test User/photo.jpg)", nil)
		}}.build(),
		"a reference XObject": doc{page: "/Resources <</XObject <</Fm0 20 0 R>>>>", objects: func(b *builder) {
			b.stream(20, "/Subtype /Form /BBox [0 0 1 1] /Ref <</F <</F (/home/Test User/original.pdf)>> /Page 1>>", []byte("q Q"))
		}}.build(),
		"a file specification with only a Unix name": doc{catalog: "/OpenAction <</S /GoToR /D [0 /Fit] /F <</Unix (/home/Test User/a.pdf)>>>>"}.build(),
		"a bare DOS name in a dictionary":            doc{page: "/Annots [<</Subtype /Link /Rect [0 0 1 1] /A <</S /GoToR /D [0 /Fit] /F <</Type /Filespec /DOS (TESTUSER.PDF)>>>>>>]"}.build(),
	} {
		t.Run(name, func(t *testing.T) {
			rep := inspect(t, data)
			wantWarning(t, rep, "external files")
			if got := valuesOf(rep, "External file reference"); len(got) == 0 {
				t.Errorf("no External file reference in %+v", rep.Findings)
			}
		})
	}
	// A bead's /F is a dictionary with no name in it, and an annotation's is a number.
	rep := inspect(t, doc{
		catalog: "/Threads [<</F <</T 3 0 R /P 3 0 R>>>>]",
		page:    "/Annots [<</Subtype /Link /Rect [0 0 1 1] /F 4>>]",
	}.build())
	if len(rep.Warnings) != 0 {
		t.Errorf("unexpected warnings: %q", rep.Warnings)
	}
}

func TestOtherLeftovers(t *testing.T) {
	t.Run("layer user", func(t *testing.T) {
		out, rep := clean(t, doc{catalog: "/OCProperties <</OCGs [20 0 R] /D <<>>>>", objects: func(b *builder) {
			b.obj(20, "<</Type /OCG /Name (Kept) /Usage <</User <</Type /Ind /Name ("+hiddenSecret+")>>>>>>")
		}}.build(), core.Options{})
		if bytes.Contains(out, []byte(hiddenSecret)) {
			t.Errorf("the layer's user survived:\n%s", out)
		}
		if got := valueOf(t, rep, "Layer user"); got != hiddenSecret {
			t.Errorf("Layer user = %q", got)
		}
		if !bytes.Contains(out, []byte("(Kept)")) {
			t.Errorf("the layer lost its name:\n%s", out)
		}
	})
	t.Run("rich text field value", func(t *testing.T) {
		rep := inspect(t, doc{catalog: "/AcroForm <</Fields [20 0 R]>>", page: "/Annots [20 0 R]", objects: func(b *builder) {
			b.obj(20, "<</FT /Tx /Subtype /Widget /Rect [0 0 1 1] /T (x) /RV (<body>"+hiddenSecret+"</body>)>>")
		}}.build())
		wantWarning(t, rep, "form field values")
	})
	t.Run("signature whose type is inherited", func(t *testing.T) {
		out, rep := clean(t, doc{catalog: "/AcroForm <</Fields [20 0 R] /SigFlags 3>>", page: "/Annots [21 0 R]", objects: func(b *builder) {
			b.obj(20, "<</FT /Sig /T (parent) /Kids [21 0 R]>>")
			b.obj(21, "<</Subtype /Widget /Parent 20 0 R /Rect [0 0 1 1] /V 22 0 R>>")
			b.obj(22, "<</Filter /Adobe.PPKLite /Name ("+hiddenSecret+")>>")
		}}.build(), core.Options{})
		if bytes.Contains(out, []byte(hiddenSecret)) {
			t.Errorf("the signer survived:\n%s", out)
		}
		if got := valueOf(t, rep, "Signer name"); got != hiddenSecret {
			t.Errorf("Signer name = %q", got)
		}
	})
	t.Run("a parent chain that loops", func(t *testing.T) {
		inspect(t, doc{catalog: "/AcroForm <</Fields [20 0 R]>>", objects: func(b *builder) {
			b.obj(20, "<</Subtype /Widget /Parent 21 0 R /V (x)>>")
			b.obj(21, "<</Parent 20 0 R>>")
		}}.build())
	})
	t.Run("embedded media", func(t *testing.T) {
		for _, annot := range []string{"/Subtype /3D /3DD 20 0 R", "/Subtype /Sound /Sound 20 0 R", "/Subtype /Movie /Movie <</F (a)>>",
			"/Subtype /RichMedia /RichMediaContent <<>>"} {
			rep := inspect(t, doc{page: "/Annots [<</Rect [0 0 1 1] " + annot + ">>]", objects: func(b *builder) {
				b.stream(20, "", []byte(hiddenSecret))
			}}.build())
			wantWarning(t, rep, "sound, video or 3D")
		}
	})
	t.Run("related files", func(t *testing.T) {
		out, rep := clean(t, doc{catalog: "/OpenAction <</S /GoToR /D [0 /Fit] /F <</F (a) /RF <</F [(a) 20 0 R]>>>>>>", objects: func(b *builder) {
			b.stream(20, "", []byte(hiddenSecret))
		}}.build(), core.Options{})
		if bytes.Contains(out, []byte(hiddenSecret)) {
			t.Errorf("a related file's data survived:\n%s", out)
		}
		if len(valuesOf(rep, "Attachment")) == 0 {
			t.Errorf("no finding in %+v", rep.Findings)
		}
	})
	t.Run("web capture sources", func(t *testing.T) {
		out, rep := clean(t, doc{catalog: "/Names <</URLS <</Names [(http://" + hiddenSecret + ".invalid/) 3 0 R]>> /IDS <</Names [<00> 3 0 R]>>>>"}.build(), core.Options{})
		if bytes.Contains(out, []byte(hiddenSecret)) || bytes.Contains(out, []byte("/IDS")) {
			t.Errorf("web capture data survived:\n%s", out)
		}
		if len(valuesOf(rep, "Web capture data")) == 0 {
			t.Errorf("no finding in %+v", rep.Findings)
		}
	})
	t.Run("colour profiles and font programs are reported as kept", func(t *testing.T) {
		data := doc{
			page: "/Resources <</Font <</F2 20 0 R>> /ColorSpace <</CS0 [/ICCBased 22 0 R]>>>>",
			objects: func(b *builder) {
				b.obj(20, "<</Type /Font /Subtype /TrueType /BaseFont /X /FontDescriptor 21 0 R>>")
				b.obj(21, "<</Type /FontDescriptor /FontName /X /Flags 32 /FontFile2 23 0 R>>")
				b.stream(22, "/N 3", []byte{0})
				b.stream(23, "/Length1 1", []byte{0})
			},
		}.build()
		for _, rep := range []*core.Report{inspect(t, data), inspect(t, cleanOK(t, data))} {
			for _, field := range []string{"ICC colour profile", "Embedded font program"} {
				i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == field })
				if i < 0 || rep.Findings[i].Action != core.ActionRetain || rep.Findings[i].Reason == "" {
					t.Errorf("%s not reported as retained: %+v", field, rep.Findings)
				}
			}
		}
	})
}

// A zlib stream's header and checksum are not hidden data, whatever they look like,
// and a stream with neither must not lose its last bytes.
func TestFlateEndIsExact(t *testing.T) {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.NoCompression)
	_, _ = w.Write([]byte(pageBody))
	_ = w.Close()
	stored := buf.Bytes()
	out, rep := clean(t, contentDoc("/Filter /FlateDecode", stored), core.Options{})
	if !bytes.Contains(out, stored) || len(rep.Findings) != 0 {
		t.Errorf("a stored-block stream changed: %+v\n%q", rep.Findings, out)
	}
	bare := stored[2 : len(stored)-4]
	out, rep = clean(t, contentDoc("/Filter /FlateDecode", slices.Concat(bare, []byte("abc"))), core.Options{})
	if bytes.Contains(out, []byte("abc\nendstream")) || len(valuesOf(rep, "Data after a stream's end")) == 0 {
		t.Errorf("bare deflate has no checksum, so nothing after it is excused: %+v\n%q", rep.Findings, out)
	}
}

// Clean prints Field and Where, and warnings and errors, on every run, so that a log
// can be kept without recreating what was removed. File text belongs in Value alone.
func TestFileTextStaysOutOfWhatCleanPrints(t *testing.T) {
	const m = "ZZMark"
	data := doc{
		catalog: "/Metadata 20 0 R /Names <</EmbeddedFiles <</Names [(" + m + "TreeName) 21 0 R]>>>> /AcroForm 25 0 R /" + m + "CatKey (" + m + "CatVal) " +
			"/PieceInfo <</" + m + "Piece <</Private 1>>>> /OCProperties <</OCGs [28 0 R] /D <<>>>> " +
			"/OpenAction <</S /GoToR /D [0 /Fit] /F (" + m + "Path) /" + m + "ActKey (" + m + "ActVal)>>",
		page: "/Annots [24 0 R 26 0 R] /" + m + "PageKey (" + m + "PageVal) /Group <</S /Transparency /" + m + "Group 29 0 R>> " +
			"/Resources <</Font <</F1 5 0 R /" + m + "Font 30 0 R>> /" + m + "ResKey (" + m + "ResVal)>>",
		trailer: "/Info 10 0 R /" + m + "Trailer (" + m + "TrailVal)",
		objects: func(b *builder) {
			b.obj(10, "<</"+m+"InfoKey ("+m+"InfoVal) /Author ("+m+"Author)>>")
			b.stream(20, "/Type /Metadata /Subtype /XML", []byte("<x:xmpmeta><"+m+"Prop>"+m+"PropVal</"+m+"Prop></x:xmpmeta>"))
			b.obj(21, "<</Type /Filespec /F ("+m+"AttachF) /UF ("+m+"AttachUF) /EF <</F 22 0 R>>>>")
			b.stream(22, "/Type /EmbeddedFile", []byte("x"))
			b.obj(24, "<</Type /Annot /Subtype /Text /Rect [0 0 1 1] /T ("+m+"AnnotT) /NM ("+m+"AnnotNM) /M ("+m+"AnnotM) "+
				"/Contents ("+m+"AnnotText) /"+m+"AnnotKey ("+m+"AnnotVal)>>")
			b.obj(25, "<</Fields [26 0 R 27 0 R]>>")
			b.obj(26, "<</FT /Tx /Subtype /Widget /Rect [0 0 1 1] /T ("+m+"FieldT) /V ("+m+"FieldV)>>")
			b.obj(27, "<</FT /Sig /T ("+m+"SigT) /V <</Type /Sig /Name ("+m+"Signer) /Location ("+m+"Place) /Reason ("+m+"Why) "+
				"/ContactInfo ("+m+"Mail) /M ("+m+"When) /ByteRange [0 1 2 3] /Contents <00>>>>>")
			b.obj(28, "<</Type /OCG /Name ("+m+"Layer) /Usage <</CreatorInfo <</Creator ("+m+"App)>> /User <</Name ("+m+"User)>>>>>>")
			b.stream(29, "/Filter /"+m+"Filter /"+m+"StreamKey ("+m+"StreamVal)", []byte("x"))
			b.obj(30, "<</Type /Font /Subtype /Type1 /BaseFont /"+m+"Base /"+m+"FontKey ("+m+"FontVal) /FontDescriptor 31 0 R>>")
			b.obj(31, "<</Type /FontDescriptor /FontName /"+m+"Base /Flags 32 /"+m+"DescKey ("+m+"DescVal) /FontFile2 32 0 R>>")
			b.stream(32, "/Length1 1", []byte{0})
		},
	}.build()
	out, cleaned := clean(t, data, core.Options{})
	for name, rep := range map[string]*core.Report{"Inspect": inspect(t, data), "Clean": cleaned, "Inspect on the output": inspect(t, out)} {
		for _, f := range rep.Findings {
			for _, s := range []string{f.Field, f.Where, f.Reason} {
				if strings.Contains(s, m) {
					t.Errorf("%s: printed text %q holds file text, in finding %+v", name, s, f)
				}
			}
		}
		for _, w := range rep.Warnings {
			if strings.Contains(w, m) {
				t.Errorf("%s: warning %q holds file text", name, w)
			}
		}
	}
	// The search above means nothing unless the text was there to be found.
	if got := valueOf(t, cleaned, "Custom metadata"); !strings.Contains(got, m+"InfoKey") {
		t.Errorf("Custom metadata = %q, want the key in Value", got)
	}

	// A refusal is printed too.
	objectStream := func(filter string) []byte {
		b := newBuilder("1.5")
		rows := make(map[int][3]int)
		b.stream(4, "", []byte(pageBody))
		rows[4] = [3]int{1, b.offsets[4], 0}
		b.stream(6, "/Type /ObjStm /N 1 /First 4 /Filter "+filter, []byte("1 0 <<>>"))
		rows[6] = [3]int{1, b.offsets[6], 0}
		rows[1] = [3]int{2, 6, 0}
		at := b.xrefStream(7, 8, rows, "/Root 1 0 R")
		fmt.Fprintf(&b.buf, "startxref\n%d\n%%%%EOF\n", at)
		return b.bytes()
	}
	for name, refused := range map[string][]byte{
		"an object stream's unknown filter":   objectStream("/" + m + "Filter"),
		"a name where a filter should be":     doc{page: "/Group <</A 20 0 R>>", objects: func(b *builder) { b.stream(20, "/Filter [("+m+")]", nil) }}.build(),
		"a token where a value should be":     doc{catalog: "/Lang " + m}.build(),
		"an encryption dictionary":            doc{trailer: "/Encrypt <</Filter /" + m + ">>"}.build(),
		"text in a page tree":                 doc{objects: func(b *builder) { b.obj(2, "("+m+")") }}.build(),
		"text where the catalog should be":    doc{objects: func(b *builder) { b.obj(1, "("+m+")") }}.build(),
		"a damaged JPEG holding the text":     doc{page: "/Resources <</XObject <</Im0 20 0 R>>>>", objects: func(b *builder) { b.stream(20, "/Subtype /Image /Filter /DCTDecode", append([]byte{0xFF, 0xD8}, m...)) }}.build(),
		"a second name for a stream's filter": doc{page: "/Group <</A 20 0 R>>", objects: func(b *builder) { b.stream(20, "/Filter /"+m+" /F /"+m, nil) }}.build(),
	} {
		t.Run(name, func(t *testing.T) {
			for _, opts := range []core.Options{{}, {Strict: true}} {
				_, err := testHandler.Inspect(context.Background(), core.BytesSource(refused), opts)
				if err != nil && strings.Contains(err.Error(), m) {
					t.Errorf("the error %q holds file text", err)
				}
				var sink bytes.Buffer
				_, err = testHandler.Clean(context.Background(), core.BytesSource(refused), &sink, opts)
				if err != nil && strings.Contains(err.Error(), m) {
					t.Errorf("the error %q holds file text", err)
				}
			}
		})
	}
	// The standard decoders quote the byte they did not like.
	d := newDocument(context.Background(), nil, defaultLimits)
	dict := &pdfDict{}
	dict.set("Filter", pdfName("ASCIIHexDecode"))
	if _, err := d.decodeStream(&pdfStream{dict: dict, raw: []byte(m)}); err != nil && strings.Contains(err.Error(), "'Z'") {
		t.Errorf("the error %q holds file text", err)
	}
}

// A form's default resources still hold /Encoding, from PDF 1.0, and readers use it
// to build field appearances, so it must not go with a producer's private categories.
func TestFormDefaultResourcesKeepEncoding(t *testing.T) {
	out, rep := clean(t, doc{catalog: "/AcroForm <</Fields [] /DR <</Encoding <</PDFDocEncoding 20 0 R>> /Font <</Helv 5 0 R>>>> /DA (/Helv 0 Tf 0 g)>>", objects: func(b *builder) {
		b.obj(20, "<</Type /Encoding /Differences [24 /breve]>>")
	}}.build(), core.Options{})
	if !bytes.Contains(out, []byte("/Encoding <</PDFDocEncoding")) || !bytes.Contains(out, []byte("/Differences")) {
		t.Errorf("the form lost its encoding:\n%s", out)
	}
	if len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
		t.Errorf("reported something: %+v %q", rep.Findings, rep.Warnings)
	}
}

// Filter parameters are spec keys like any others, and every predictor image has them.
func TestFilterParametersAreRecognised(t *testing.T) {
	_, rep := clean(t, doc{page: "/Group <</S /Transparency /Hidden 20 0 R>>", objects: func(b *builder) {
		b.stream(20, "/Filter /FlateDecode /DecodeParms <</Predictor 12 /Colors 1 /BitsPerComponent 8 /Columns 2 /EarlyChange 1>>", compress([]byte{0, 1, 2}))
	}}.build(), core.Options{})
	if got := valuesOf(rep, "Unrecognised key"); len(got) != 1 || got[0] != "Hidden" {
		t.Errorf("Unrecognised key = %q", got)
	}
}

// PostScript XObjects are drawn too, and PostScript is full of comments.
func TestPostScriptXObject(t *testing.T) {
	_, rep := clean(t, doc{page: "/Resources <</XObject <</Ps0 20 0 R>>>>", objects: func(b *builder) {
		b.stream(20, "/Subtype /PS /Filter /FlateDecode", compress([]byte("%%Creator: "+hiddenSecret+"\n")))
	}}.build(), core.Options{})
	wantWarning(t, rep, "comments")
}

// A map's entries lead to resources, so a string in one is not a resource's name.
func TestTextInANameMapWarns(t *testing.T) {
	for name, data := range map[string][]byte{
		"a font map":      doc{page: "/Resources <</Font <</F1 5 0 R /Who (" + hiddenSecret + ")>>>>"}.build(),
		"an indirect map": doc{page: "/Resources <</XObject 20 0 R>>", objects: func(b *builder) { b.obj(20, "<</Who ("+hiddenSecret+")>>") }}.build(),
		"destinations":    doc{catalog: "/Dests <</Who (" + hiddenSecret + ")>>"}.build(),
		"extensions":      doc{catalog: "/Extensions <</Who (" + hiddenSecret + ")>>"}.build(),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			wantWarning(t, rep, "does not recognise")
		})
	}
}

// Being named as a resource map must not let a signature or an attachment past the
// rules that remove them whole. A font called /EF is one thing, and a dictionary
// holding an embedded file is another.
func TestRemovedShapesCannotHideAsNameMaps(t *testing.T) {
	asFontMap := "/Resources <</Font 20 0 R>>"
	for name, body := range map[string]string{
		"signature":       "<</ByteRange [0 1 2 3] /Name (" + hiddenSecret + ")>>",
		"signature, late": "<</SubFilter /adbe.pkcs7.detached /Contents <00> /Name (" + hiddenSecret + ")>>",
		"attachment":      "<</EF <</F 21 0 R>> /UF (" + hiddenSecret + ")>>",
		"related file":    "<</RF <</UF [(a) 21 0 R]>> /F (" + hiddenSecret + ")>>",
		"annotation":      "<</Subtype /FileAttachment /Name (" + hiddenSecret + ")>>",
	} {
		t.Run(name, func(t *testing.T) {
			out := cleanOK(t, doc{page: asFontMap, objects: func(b *builder) {
				b.obj(20, body)
				b.stream(21, "", []byte(hiddenSecret))
			}}.build())
			if bytes.Contains(out, []byte(hiddenSecret)) {
				t.Errorf("it survived by being a map:\n%s", out)
			}
		})
	}
}

// PDF lets a page's content be divided between streams at any token, so a dictionary
// that runs off the end of one may go on in the next.
func TestInlineImageSplitAcrossContentStreams(t *testing.T) {
	for name, parts := range map[string][2]string{
		"after the operator":    {"q BI", "/W 1 /H 1 /F /DCT ID \xFF\xD8\xFF\xD9 EI Q"},
		"inside the keys":       {"q BI /W 1 /H 1", "/F /DCT ID \xFF\xD8\xFF\xD9 EI Q"},
		"between key and value": {"q BI /W 1 /H 1 /F", "/DCT ID \xFF\xD8\xFF\xD9 EI Q"},
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, doc{page: "/Contents [4 0 R 20 0 R]", contents: func(b *builder) {
				b.stream(4, "/Filter /FlateDecode", compress([]byte(parts[0])))
			}, objects: func(b *builder) {
				b.stream(20, "/Filter /FlateDecode", compress([]byte(parts[1])))
			}}.build(), core.Options{})
			wantWarning(t, rep, "inline")
		})
	}
}

// What a stream says about itself must not switch off the search, since a page draws
// whatever /Contents points at and a glyph has no key to say what it is.
func TestInlineImageInAStreamClaimingToBeSomethingElse(t *testing.T) {
	body := compress([]byte("q BI /W 1 /H 1 /F /DCT ID \xFF\xD8\xFF\xD9 EI Q"))
	for name, claim := range map[string]string{"a font": "/Length1 1 ", "an image": "/Subtype /Image /Width 1 /Height 1 /BitsPerComponent 8 /ColorSpace /DeviceGray "} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, contentDoc(claim+"/Filter /FlateDecode", body), core.Options{})
			wantWarning(t, rep, "inline")
		})
	}
}

func TestCryptFilterNamedIndirectly(t *testing.T) {
	_, rep := clean(t, doc{page: privateStream, objects: func(b *builder) {
		b.stream(20, "/Filter /Crypt /DecodeParms <</Name 21 0 R>>", []byte("Test Opaque Bytes"))
		b.obj(21, "/Vendor")
	}}.build(), core.Options{})
	wantWarning(t, rep, "cannot look inside")
}

// A predictor with one-byte rows costs a good deal more per byte than decompression
// does, so a small bomb under one must reach the cap that much sooner.
func TestPredictorRowsCountTowardTheCap(t *testing.T) {
	small := defaultLimits
	small.maxScanned = 8 << 20
	h := handler{limits: small}
	bomb := compress(make([]byte, 1<<20))
	for name, parms := range map[string]string{"PNG": "<</Predictor 12 /Columns 1>>", "TIFF": "<</Predictor 2 /Columns 1>>"} {
		t.Run(name, func(t *testing.T) {
			data := streamDoc("/Filter /FlateDecode /DecodeParms "+parms, bomb)
			if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); !errors.Is(err, core.ErrTooLarge) {
				t.Errorf("err = %v, want ErrTooLarge", err)
			}
		})
	}
	// Rows of an ordinary width cost next to nothing.
	data := streamDoc("/Filter /FlateDecode /DecodeParms <</Predictor 12 /Columns 1023>>", bomb)
	if _, err := h.Inspect(context.Background(), core.BytesSource(data), core.Options{}); err != nil {
		t.Errorf("an ordinary stream was refused: %v", err)
	}
}

// A warning that a key was kept must be true: this key went, with what it led to.
func TestRemovedKeyIsNotCalledKept(t *testing.T) {
	_, rep := clean(t, streamDoc("/Filter /FlateDecode", compress(coretest.FakeXMP())), core.Options{})
	if got := valuesOf(rep, "Unrecognised key"); len(got) != 0 {
		t.Errorf("a key that was removed is reported as kept: %q", got)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("unexpected warnings: %q", rep.Warnings)
	}
}

// Keys that belong only to what is always removed are not kept by being in a
// specification: if one is still there, it is somewhere nothing explains it.
func TestDatesAndNamesOutOfPlaceWarn(t *testing.T) {
	for _, key := range []string{"CreationDate", "ModDate", "Creator", "Private", "Prop_Build", "CheckSum"} {
		t.Run(key, func(t *testing.T) {
			_, rep := clean(t, doc{page: "/Group <</S /Transparency /" + key + " (" + hiddenSecret + ")>>"}.build(), core.Options{})
			wantWarning(t, rep, "does not recognise")
		})
	}
}
