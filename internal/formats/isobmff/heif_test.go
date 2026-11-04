package isobmff

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// phonePhoto is shaped like an iPhone photo: a primary image, a thumbnail, Exif
// with the Apple signature, XMP, an ICC profile, rotation and a depth image.
func phonePhoto() heifSpec {
	return heifSpec{
		ilocVersion: 0,
		offsetSize:  4,
		lengthSize:  4,
		primary:     1,
		items: []itemSpec{
			{id: 1, typ: "hvc1", data: primaryData, props: []uint16{1, 2, 3, 4, 5}},
			{id: 2, typ: "hvc1", data: thumbnailData, props: []uint16{6, 2}},
			{id: 3, typ: "Exif", data: exifItem(coretest.FakeEXIF(6))},
			{id: 4, typ: "mime", content: "application/rdf+xml", data: coretest.FakeXMP()},
			{id: 5, typ: "hvc1", data: auxData, hidden: true, props: []uint16{1, 7}},
		},
		refs: []itemRef{
			{typ: "thmb", from: 2, to: []uint32{1}},
			{typ: "cdsc", from: 3, to: []uint32{1}},
			{typ: "cdsc", from: 4, to: []uint32{1}},
			{typ: "auxl", from: 5, to: []uint32{1}},
		},
		properties: standardProperties(),
	}
}

func TestHEIFContract(t *testing.T) {
	fields := slices.Concat(coretest.FakeEXIFFields(), coretest.FakeXMPFields(), []string{"Orientation", "Handler name"})
	photoSecrets := slices.Concat(coretest.FakeEXIFSecrets(), coretest.FakeXMPSecrets(), asBytes("THUMBNAIL", "Exif\x00\x00", "Test Handler Name", "THUMB DECODER", "infeExif"))
	kept := [][]byte{primaryData, auxData, []byte("PRIMARY DECODER CONFIG"), []byte("irot"), []byte("auxC")}

	phone := phonePhoto()

	avif := phonePhoto()
	avif.brand = "avif"
	avif.items[0].typ = "av01"
	avif.items[1].typ = "av01"
	avif.items[4].typ = "av01"

	iloc1 := phonePhoto()
	iloc1.ilocVersion = 1
	iloc1.indexSize = 4

	iloc2 := phonePhoto()
	iloc2.ilocVersion = 2
	iloc2.offsetSize, iloc2.lengthSize, iloc2.baseSize = 8, 8, 8
	iloc2.indexSize = 8

	baseOnly := phonePhoto()
	baseOnly.offsetSize = 0
	baseOnly.baseSize = 8

	// The grid is in idat, which moves when the tables before it shrink. Its
	// offset is relative to idat, so it must still read correctly.
	grid := phonePhoto()
	grid.ilocVersion = 1
	grid.items = []itemSpec{
		{id: 1, typ: "grid", data: []byte("GRID CONFIG"), inIdat: true, props: []uint16{2, 3}},
		{id: 2, typ: "hvc1", data: []byte("TILE ONE DATA"), hidden: true, props: []uint16{1}},
		{id: 3, typ: "hvc1", data: []byte("TILE TWO DATA"), hidden: true, props: []uint16{1}},
		{id: 4, typ: "Exif", data: exifItem(coretest.FakeEXIF(1)), inIdat: true},
		{id: 5, typ: "grid", data: []byte("THUMB GRID"), inIdat: true, props: []uint16{2}},
		{id: 6, typ: "hvc1", data: []byte("THUMB TILE SECRET"), hidden: true, props: []uint16{6}},
	}
	grid.refs = []itemRef{
		{typ: "dimg", from: 1, to: []uint32{2, 3}},
		{typ: "cdsc", from: 4, to: []uint32{1}},
		{typ: "thmb", from: 5, to: []uint32{1}},
		{typ: "dimg", from: 5, to: []uint32{6}},
	}
	grid.idatSuffix = []byte("Test Orphan Idat")

	orphans := phonePhoto()
	orphans.mdatSuffix = []byte("Test Orphan Mdat")
	orphans.before = bx("free", []byte("Test Stale Free"))
	orphans.after = []byte("\x12\x34\x56\x78\xFFTest Trailer")
	orphans.metaExtra = cat(bx("free", []byte("Test Meta Free")), bx("uuid", []byte("Test Meta UUID 0123456789")))

	named := phonePhoto()
	named.items[0].name = "Test Item Name"

	large := phonePhoto()
	large.largeMeta = true

	uri := phonePhoto()
	uri.items = append(uri.items,
		itemSpec{id: 6, typ: "uri ", name: "", data: []byte("Test URI Payload")},
		itemSpec{id: 7, typ: "vend", data: []byte("Test Vendor Item")},
		itemSpec{id: 8, typ: "mime", content: "text/plain", data: []byte("Test Text Item")},
	)

	userDesc := phonePhoto()
	userDesc.properties = append(userDesc.properties, fullBox("udes", 0, 0, []byte("en\x00Test Desc Name\x00Test Desc Text\x00\x00")))
	userDesc.items[0].props = append(userDesc.items[0].props, 8)

	deadProperty := phonePhoto()
	deadProperty.properties = append(deadProperty.properties, bx("hvcC", []byte("Test Unused Property")))

	wide := phonePhoto()
	wide.wideIDs = true
	wide.ilocVersion = 2
	wide.items[0].id = 0x10001 // an ID that does not fit in 16 bits
	wide.primary = 0x10001
	wide.refs = []itemRef{
		{typ: "thmb", from: 2, to: []uint32{0x10001}},
		{typ: "cdsc", from: 3, to: []uint32{0x10001}},
		{typ: "cdsc", from: 4, to: []uint32{0x10001}},
		{typ: "auxl", from: 5, to: []uint32{0x10001}},
	}

	legacyInfe := phonePhoto()
	legacyInfe.extraInfes = [][]byte{
		fullBox("infe", 0, 0, be16(20), be16(0), []byte("Test Legacy Name\x00"), []byte("text/plain\x00\x00")),
		fullBox("infe", 1, 0, be16(21), be16(0), []byte("Test Legacy Two\x00"), []byte("text/plain\x00\x00")),
	}

	motion := phonePhoto()
	motion.after = bx("mpvd", []byte("Test Motion Photo Video"))

	sequence := phonePhoto()
	sequence.moov = bx("moov", fullBox("mvhd", 0, 0, be32(fakeTime), be32(fakeTime), make([]byte, 88)), bx("udta", bx("\xa9xyz", qtText("+48.8584+002.2945/"))))

	cases := []coretest.Case{
		{
			Name:           "phone photo",
			Data:           phone.build(),
			WantFields:     slices.Concat(fields, []string{"Embedded thumbnail"}),
			WantRetained:   []string{"ICC colour profile", "Auxiliary image"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "AVIF",
			Data:           avif.build(),
			WantFields:     []string{"GPS coordinates", "Embedded thumbnail"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "iloc version 1 with index fields",
			Data:           iloc1.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "iloc version 2 with 64-bit fields",
			Data:           iloc2.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "iloc with only base offsets",
			Data:           baseOnly.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "grid in idat with a grid thumbnail",
			Data:           grid.build(),
			WantFields:     []string{"GPS coordinates", "Embedded thumbnail", "Unreferenced item data"},
			MustNotContain: slices.Concat(coretest.FakeEXIFSecrets(), asBytes("THUMB GRID", "THUMB TILE SECRET", "Test Orphan Idat", "THUMB DECODER")),
			MustContain:    asBytes("GRID CONFIG", "TILE ONE DATA", "TILE TWO DATA", "PRIMARY DECODER CONFIG"),
		},
		{
			Name:           "orphaned bytes, free space, trailer and boxes meta does not allow",
			Data:           orphans.build(),
			WantFields:     []string{"Unreferenced media data", "Data in free space", "Trailing data", "Vendor UUID box"},
			MustNotContain: slices.Concat(photoSecrets, asBytes("Test Orphan", "Test Stale", "Test Trailer", "Test Meta")),
			MustContain:    kept,
		},
		{
			Name:           "item name",
			Data:           named.build(),
			WantFields:     []string{"Item name"},
			MustNotContain: asBytes("Test Item Name"),
			MustContain:    kept,
		},
		{
			Name:           "64-bit meta header",
			Data:           large.build(),
			WantFields:     []string{"GPS coordinates"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "other item types",
			Data:           uri.build(),
			WantFields:     []string{"URI item", "Unknown item", "Embedded data item"},
			MustNotContain: asBytes("Test URI", "Test Vendor Item", "Test Text Item", "vend", "text/plain"),
			MustContain:    kept,
		},
		{
			Name:           "user description property",
			Data:           userDesc.build(),
			WantFields:     []string{"Image description"},
			MustNotContain: asBytes("Test Desc Name", "Test Desc Text", "udes"),
			MustContain:    kept,
		},
		{
			Name:           "property nothing uses",
			Data:           deadProperty.build(),
			WantFields:     []string{"Unreferenced image property"},
			MustNotContain: asBytes("Test Unused Property"),
			MustContain:    kept,
		},
		{
			Name:           "32-bit item IDs and 16-bit property indexes",
			Data:           wide.build(),
			WantFields:     []string{"GPS coordinates", "Embedded thumbnail"},
			WantRetained:   []string{"ICC colour profile", "Auxiliary image"},
			MustNotContain: photoSecrets,
			MustContain:    kept,
		},
		{
			Name:           "infe versions 0 and 1 are file-like items",
			Data:           legacyInfe.build(),
			WantFields:     []string{"Embedded data item"},
			MustNotContain: asBytes("Test Legacy", "text/plain"),
			MustContain:    kept,
		},
		{
			Name:           "motion photo video",
			Data:           motion.build(),
			WantFields:     []string{"Motion photo video"},
			MustNotContain: asBytes("Test Motion Photo Video", "mpvd"),
			MustContain:    kept,
		},
		{
			Name:           "with a moov as well",
			Data:           sequence.build(),
			WantFields:     []string{"Capture time", "GPS coordinates"},
			MustNotContain: asBytes("+48.8584"),
			MustContain:    kept,
		},
	}
	coretest.Contract(t, heifHandler, cases)
}

// TestHEIFLayoutIsStable checks the technique: meta keeps its size, so nothing
// after it moves, and the rebuilt iloc still points at the right bytes.
func TestHEIFLayoutIsStable(t *testing.T) {
	for _, spec := range []heifSpec{phonePhoto(), func() heifSpec {
		s := phonePhoto()
		s.ilocVersion = 2
		s.baseSize, s.offsetSize, s.lengthSize = 4, 8, 8
		return s
	}()} {
		data := spec.build()
		out, _ := mustClean(t, heifHandler, data, core.Options{})
		if len(out) != len(data) {
			t.Fatalf("size changed from %d to %d", len(data), len(out))
		}
		at := bytes.Index(data, primaryData)
		if !bytes.Equal(out[at:at+len(primaryData)], primaryData) {
			t.Fatal("the primary image moved")
		}
		hdr := parsedMeta(t, out)
		if len(hdr.iloc.entries) != 2 {
			t.Fatalf("iloc has %d entries, want 2", len(hdr.iloc.entries))
		}
		e := hdr.iloc.entries[0]
		start := e.base + e.extents[0].offset
		if got := out[start : start+e.extents[0].length]; !bytes.Equal(got, primaryData) {
			t.Errorf("iloc points at %q", got)
		}
		wantItems := []uint32{1, 5}
		var gotItems []uint32
		for _, it := range hdr.items {
			gotItems = append(gotItems, it.id)
		}
		if !slices.Equal(gotItems, wantItems) {
			t.Errorf("items are %v, want %v", gotItems, wantItems)
		}
		for _, ref := range hdr.refs {
			if ref.typ == "thmb" || ref.typ == "cdsc" {
				t.Errorf("reference %q survived", ref.typ)
			}
		}
		for _, table := range hdr.ipmas {
			for _, entry := range table.entries {
				if entry.id != 1 && entry.id != 5 {
					t.Errorf("ipma still lists item %d", entry.id)
				}
			}
		}
	}
}

// parsedMeta reads the output's meta box back through the same parsers.
func parsedMeta(t *testing.T, data []byte) *metaBox {
	t.Helper()
	src := core.BytesSource(data)
	a := &analysis{ctx: testContext(), src: src, report: &core.Report{}, seen: map[string]bool{}}
	top, _, err := a.topLevel()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range top {
		if b.typ != "meta" {
			continue
		}
		m, err := a.load(b)
		if err != nil {
			t.Fatal(err)
		}
		h := &metaBox{a: a, m: m, top: b, byID: make(map[uint32]*heifItem)}
		if err := h.parseChildren(); err != nil {
			t.Fatal(err)
		}
		return h
	}
	t.Fatal("no meta box")
	return nil
}

func TestIdatOffsetsSurviveMoving(t *testing.T) {
	spec := phonePhoto()
	spec.ilocVersion = 1
	spec.items = []itemSpec{
		{id: 1, typ: "grid", data: []byte("GRID CONFIG"), inIdat: true, props: []uint16{2}},
		{id: 2, typ: "hvc1", data: []byte("TILE DATA"), hidden: true, props: []uint16{1}},
		{id: 3, typ: "Exif", data: exifItem(coretest.FakeEXIF(1))},
		{id: 4, typ: "hvc1", data: []byte("THUMBNAIL BYTES"), props: []uint16{6}},
	}
	spec.refs = []itemRef{{typ: "dimg", from: 1, to: []uint32{2}}, {typ: "thmb", from: 4, to: []uint32{1}}}
	data := spec.build()
	inAt := bytes.Index(data, []byte("GRID CONFIG"))
	out, _ := mustClean(t, heifHandler, data, core.Options{})
	outAt := bytes.Index(out, []byte("GRID CONFIG"))
	if outAt >= inAt {
		t.Fatalf("idat did not move (%d, %d), so this test proves nothing", inAt, outAt)
	}
	h := parsedMeta(t, out)
	for _, e := range h.iloc.entries {
		if e.id == 1 {
			idat := out[h.idat.body+h.m.off : h.idat.end+h.m.off]
			x := e.extents[0]
			if got := idat[x.offset : x.offset+x.length]; string(got) != "GRID CONFIG" {
				t.Errorf("idat offset now reads %q", got)
			}
		}
	}
}

func TestStrictRemovesICCAndOrientation(t *testing.T) {
	spec := phonePhoto()
	data := spec.build()
	out, rep := mustClean(t, heifHandler, data, core.Options{Strict: true})
	if bytes.Contains(out, []byte("Test ICC Profile")) {
		t.Error("the profile survived")
	}
	for _, f := range rep.Findings {
		if f.Field == "ICC colour profile" && f.Action != core.ActionRemove {
			t.Errorf("profile reported as %s", f.Action)
		}
	}
	if !strings.Contains(describeFindings(rep), "ICC colour profile") {
		t.Errorf("strict removal not reported: %s", describeFindings(rep))
	}
	if bytes.Contains(out, []byte("irot")) {
		t.Error("strict mode kept the rotation, which --strict promises to remove")
	}
	// The primary keeps its other properties at the same indexes.
	h := parsedMeta(t, out)
	if h.ipmas[0].entries[0].id != 1 {
		t.Fatal("unexpected first ipma entry")
	}
	var indexes []uint16
	for _, assoc := range h.ipmas[0].entries[0].assocs {
		indexes = append(indexes, assoc.index)
	}
	if !slices.Equal(indexes, []uint16{1, 2, 4}) {
		t.Errorf("primary's properties are now %v", indexes)
	}
	for i, p := range h.props {
		if want := []string{"hvcC", "ispe", "free", "colr", "free", "free", "auxC"}[i]; p.box.typ != want {
			t.Errorf("property %d is %q, want %q", i+1, p.box.typ, want)
		}
	}
}

// TestHandlerPadding covers a rebuild that saves fewer bytes than a free box
// needs: strict mode dropping a single association, with nothing else to remove.
func TestHandlerPadding(t *testing.T) {
	spec := heifSpec{
		offsetSize: 4,
		lengthSize: 4,
		primary:    1,
		items: []itemSpec{
			{id: 1, typ: "hvc1", data: primaryData, props: []uint16{1, 2}},
		},
		properties: [][]byte{bx("hvcC", []byte("PRIMARY DECODER CONFIG")), coloursICC()},
	}
	coretest.Contract(t, heifHandler, []coretest.Case{{
		Name:           "strict drops one association",
		Data:           spec.build(),
		Opts:           core.Options{Strict: true},
		WantFields:     []string{"ICC colour profile"},
		MustNotContain: asBytes("Test ICC Profile"),
		MustContain:    [][]byte{primaryData},
	}})
	out, _ := mustClean(t, heifHandler, spec.build(), core.Options{Strict: true})
	if len(out) != len(spec.build()) {
		t.Errorf("size changed from %d to %d", len(spec.build()), len(out))
	}
	if h := parsedMeta(t, out); h.find("hdlr").size() == 0 {
		t.Error("no hdlr")
	}
}

// TestDerivedAuxOfThumbnail checks that an alpha plane of a removed thumbnail goes
// with it, while the same kind of image on the primary stays.
func TestAuxOfThumbnailGoes(t *testing.T) {
	spec := phonePhoto()
	spec.items = append(spec.items, itemSpec{id: 6, typ: "hvc1", data: []byte("THUMB ALPHA SECRET"), hidden: true, props: []uint16{7}})
	spec.refs = append(spec.refs, itemRef{typ: "auxl", from: 6, to: []uint32{2}})
	out, _ := mustClean(t, heifHandler, spec.build(), core.Options{})
	if bytes.Contains(out, []byte("THUMB ALPHA SECRET")) {
		t.Error("the thumbnail's auxiliary image survived")
	}
	if !bytes.Contains(out, auxData) {
		t.Error("the primary's auxiliary image was removed")
	}
}

func TestAdditionalImageStays(t *testing.T) {
	spec := phonePhoto()
	spec.items = append(spec.items, itemSpec{id: 6, typ: "hvc1", data: []byte("SECOND IMAGE IN THE BURST"), props: []uint16{1, 2}})
	out, rep := mustClean(t, heifHandler, spec.build(), core.Options{})
	if !bytes.Contains(out, []byte("SECOND IMAGE IN THE BURST")) {
		t.Error("a second image was removed")
	}
	if !strings.Contains(describeFindings(rep), "Additional image") {
		t.Errorf("not reported: %s", describeFindings(rep))
	}
}

// TestHEIFItemTableVersionTwo covers an iinf box of version 2, which ExifTool's sample
// HEIC uses. The standard gives every version but 0 a 32-bit item count.
func TestHEIFItemTableVersionTwo(t *testing.T) {
	s := phonePhoto()
	s.iinfVersion = 2
	out, _ := mustClean(t, heifHandler, s.build(), core.Options{})
	if !bytes.Contains(out, primaryData) {
		t.Error("the primary image was lost")
	}
	if bytes.Contains(out, []byte("THUMBNAIL")) {
		t.Error("the thumbnail survived")
	}
	rep, err := heifHandler.Inspect(testContext(), core.BytesSource(out), core.Options{})
	if err != nil {
		t.Fatalf("the output does not read back: %v", err)
	}
	for _, f := range rep.Findings {
		if f.Action == core.ActionRemove {
			t.Errorf("the output still has something to remove: %s", f.Field)
		}
	}
}

func TestHEIFRefused(t *testing.T) {
	mutate := func(fn func(*heifSpec)) []byte {
		s := phonePhoto()
		fn(&s)
		return s.build()
	}
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"primary is Exif", mutate(func(s *heifSpec) { s.primary = 3 }), core.ErrMalformed},
		{"primary is a thumbnail", mutate(func(s *heifSpec) { s.primary = 2 }), core.ErrMalformed},
		{"primary undeclared", mutate(func(s *heifSpec) { s.primary = 99 }), core.ErrMalformed},
		{"unknown iloc version", mutate(func(s *heifSpec) { s.ilocVersion = 3 }), core.ErrUnsupported},
		{"unknown iinf version", mutate(func(s *heifSpec) { s.iinfVersion = 3 }), core.ErrUnsupported},
		{"bad iloc field size", mutate(func(s *heifSpec) { s.offsetSize = 5 }), core.ErrMalformed},
		{"unknown essential property", mutate(func(s *heifSpec) {
			s.properties = append(s.properties, bx("zzzz", []byte{1}))
			s.items[0].props = append(s.items[0].props, 0x88)
		}), core.ErrUnsupported},
		{"property index past the end", mutate(func(s *heifSpec) { s.items[0].props = append(s.items[0].props, 99) }), core.ErrMalformed},
		{"grid built from Exif", mutate(func(s *heifSpec) { s.refs = append(s.refs, itemRef{typ: "dimg", from: 1, to: []uint32{3}}) }), core.ErrMalformed},
		{"item protection box", mutate(func(s *heifSpec) { s.metaExtra = bx("ipro", be16(0)) }), core.ErrEncrypted},
		{"two meta boxes", mutate(func(s *heifSpec) { s.after = fullBox("meta", 0, 0) }), core.ErrMalformed},
		{"Exif overlapping the image", overlappingItems(), core.ErrMalformed},
		{"kept image with no location", unlocatedImage(), core.ErrMalformed},
		{"item declared twice", duplicateItem(), core.ErrMalformed},
		{"item located twice", duplicateLocation(), core.ErrMalformed},
		{"handler is not pict", wrongHandler(), core.ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := heifHandler.Inspect(testContext(), core.BytesSource(c.data), core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Inspect gave %v, want %v", err, c.want)
			}
			if _, err := heifHandler.Clean(testContext(), core.BytesSource(c.data), &bytes.Buffer{}, core.Options{}); !errors.Is(err, c.want) {
				t.Errorf("Clean gave %v, want %v", err, c.want)
			}
		})
	}
}

// overlappingItems makes the Exif item's extent point at the primary's bytes, which
// could otherwise wipe the picture or leave the metadata.
func overlappingItems() []byte {
	spec := phonePhoto()
	data := spec.build()
	h := parseFixtureIloc(data)
	var primary, exifEntry ilocEntry
	for _, e := range h.entries {
		switch e.id {
		case 1:
			primary = e
		case 3:
			exifEntry = e
		}
	}
	// Each entry's offset is 4 bytes after its id, data reference and extent count.
	out := bytes.Clone(data)
	at := bytes.Index(out, exifEntry.raw)
	copy(out[at+6:], be32(uint32(primary.extents[0].offset)))
	return out
}

func parseFixtureIloc(data []byte) *ilocTable {
	a := bytes.Index(data, []byte("iloc"))
	size := int(be32At(data, a-4))
	table, err := parseIloc(data[a+4 : a-4+size])
	if err != nil {
		panic(err)
	}
	return table
}

func be32At(b []byte, at int) uint32 {
	return uint32(b[at])<<24 | uint32(b[at+1])<<16 | uint32(b[at+2])<<8 | uint32(b[at+3])
}

func unlocatedImage() []byte {
	spec := phonePhoto()
	spec.items = append(spec.items, itemSpec{id: 6, typ: "hvc1"})
	return spec.build()
}

func duplicateItem() []byte {
	spec := phonePhoto()
	spec.items = append(spec.items, itemSpec{id: 1, typ: "Exif", data: []byte("x")})
	return spec.build()
}

// duplicateLocation renames the Exif item's location to the primary's ID, which
// would make the Exif bytes look like part of the picture.
func duplicateLocation() []byte {
	data := phonePhoto().build()
	h := parseFixtureIloc(data)
	out := bytes.Clone(data)
	for _, e := range h.entries {
		if e.id == 3 {
			copy(out[bytes.Index(out, e.raw):], be16(1))
		}
	}
	return out
}

func wrongHandler() []byte {
	data := phonePhoto().build()
	out := bytes.Clone(data)
	at := bytes.Index(out, []byte("pict"))
	copy(out[at:], "mdta")
	return out
}

func TestHEIFRobustness(t *testing.T) {
	coretest.Robustness(t, heifHandler, phonePhoto().build())

	grid := phonePhoto()
	grid.ilocVersion = 1
	grid.indexSize = 4
	grid.items[0] = itemSpec{id: 1, typ: "grid", data: []byte("GRID"), inIdat: true, props: []uint16{2, 5}}
	grid.items = append(grid.items, itemSpec{id: 7, typ: "hvc1", data: primaryData, hidden: true, props: []uint16{1}})
	grid.refs = append(grid.refs, itemRef{typ: "dimg", from: 1, to: []uint32{7}})
	coretest.Robustness(t, heifHandler, grid.build())
}

func TestMatchKeepsHandlersApart(t *testing.T) {
	heic := phonePhoto().build()
	movie := plainMovie().build()
	imageBrandNoMeta := cat(bx("ftyp", []byte("msf1"), be32(0), []byte("msf1")), bx("moov"), bx("mdat"))
	legacy := func(first string) []byte {
		return cat(bx(first), bx("mdat"), bx("moov"))
	}
	styp := cat(bx("styp", []byte("msdh"), be32(0)), bx("moof"), bx("mdat"))
	cases := []struct {
		name string
		data []byte
		want kind
	}{
		{"HEIC", heic, kindHEIF},
		{"MP4", movie, kindMP4},
		{"image brand but no meta", imageBrandNoMeta, kindMP4},
		{"legacy wide", legacy("wide"), kindMP4},
		{"legacy free", legacy("free"), kindMP4},
		{"legacy skip", legacy("skip"), kindMP4},
		{"legacy mdat", cat(bx("mdat", []byte("x")), bx("moov")), kindMP4},
		{"legacy moov", cat(bx("moov"), bx("mdat")), kindMP4},
		{"free without a moov", cat(bx("free"), bx("mdat")), kindNone},
		{"segment with no ftyp", styp, kindNone},
		{"too short", []byte("ftyp"), kindNone},
		{"empty", nil, kindNone},
		{"not a box", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), kindNone},
		{"ftyp too short for a brand", bx("ftyp", []byte("isom")), kindNone},
		{"ftyp implausibly long", bx("ftyp", make([]byte, maxBrandBox+4)), kindNone},
		{"JPEG 2000", cat(bx("jP  ", be32(0x0D0A870A)), bx("ftyp", []byte("jp2 "), be32(0), []byte("jp2 "))), kindNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := core.BytesSource(c.data)
			head := c.data[:min(len(c.data), core.HeadSize)]
			gotHEIF := heifHandler.Match(head, src)
			gotMP4 := mp4Handler.Match(head, src)
			if gotHEIF && gotMP4 {
				t.Fatal("both handlers matched")
			}
			if gotHEIF != (c.want == kindHEIF) || gotMP4 != (c.want == kindMP4) {
				t.Errorf("heif=%v mp4=%v, want %v", gotHEIF, gotMP4, c.want)
			}
		})
	}
}

func TestReportedFormat(t *testing.T) {
	cases := []struct {
		brand, want string
	}{
		{"heic", "heic"},
		{"avif", "avif"},
		{"mif1", "heif"},
	}
	for _, c := range cases {
		spec := phonePhoto()
		spec.brand = c.brand
		rep, err := heifHandler.Inspect(testContext(), core.BytesSource(spec.build()), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Format != c.want {
			t.Errorf("brand %q reported as %q, want %q", c.brand, rep.Format, c.want)
		}
	}
	movies := []struct {
		major, want string
	}{
		{"isom", "mp4"}, {"qt  ", "mov"}, {"M4A ", "m4a"}, {"M4B ", "m4b"}, {"M4V ", "m4v"}, {"3gp5", "3gp"},
	}
	for _, c := range movies {
		spec := plainMovie()
		spec.ftyp = bx("ftyp", []byte(c.major), be32(0), []byte("isom"))
		rep, err := mp4Handler.Inspect(testContext(), core.BytesSource(spec.build()), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Format != c.want {
			t.Errorf("brand %q reported as %q, want %q", c.major, rep.Format, c.want)
		}
	}
}

func TestRegistered(t *testing.T) {
	ids := map[string]bool{}
	for _, h := range core.Handlers() {
		ids[h.Format().ID] = true
	}
	if !ids["heif"] || !ids["mp4"] {
		t.Errorf("registered formats are %v", ids)
	}
	heic := phonePhoto().build()
	h, err := core.Detect(core.BytesSource(heic))
	if err != nil || h.Format().ID != "heif" {
		t.Errorf("Detect gave %v, %v", h, err)
	}
	movie := plainMovie().build()
	h, err = core.Detect(core.BytesSource(movie))
	if err != nil || h.Format().ID != "mp4" {
		t.Errorf("Detect gave %v, %v", h, err)
	}
}
