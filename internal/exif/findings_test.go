package exif

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

func utf16Bytes(order binary.ByteOrder, s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, unit := range units {
		order.PutUint16(out[2*i:], unit)
	}
	return out
}

func gpsBlock(order binary.ByteOrder, entries ...coretest.Entry) []byte {
	return (&coretest.IFD{GPS: &coretest.IFD{Entries: entries}}).Encode(order)
}

func TestFakeBlockReportsEverything(t *testing.T) {
	const (
		ifd0 = "APP1/Exif/IFD0"
		sub  = "APP1/Exif/Exif"
		gps  = "APP1/Exif/GPS"
		ifd1 = "APP1/Exif/IFD1"
		top  = "APP1/Exif"
	)
	want := []core.Finding{
		{Field: "GPS coordinates", Value: coretest.EiffelTower, Where: gps, Category: core.CatLocation, Risk: core.RiskHigh},
		{Field: "GPS altitude", Value: "330.0 m", Where: gps, Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "GPS timestamp", Value: "04:05:06 UTC", Where: gps, Category: core.CatTime, Risk: core.RiskMed},
		{Field: "GPS date", Value: "2001:02:03", Where: gps, Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Other GPS tags", Value: "1 tag", Where: top, Category: core.CatLocation, Risk: core.RiskMed},

		{Field: "Artist", Value: "Test Author", Where: ifd0, Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "XP author", Value: "Test XP Author", Where: ifd0, Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Copyright", Value: "Test Copyright", Where: ifd0, Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Camera make", Value: "Test Make", Where: ifd0, Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Camera model", Value: "Test Model", Where: ifd0, Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Host computer", Value: "Test Host", Where: ifd0, Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Date modified", Value: "2001:02:03 04:05:06", Where: ifd0, Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Software", Value: "Test Software", Where: ifd0, Category: core.CatSoftware, Risk: core.RiskLow},
		{Field: "Image description", Value: "Test Description", Where: ifd0, Category: core.CatContent, Risk: core.RiskMed},
		{Field: "XP comment", Value: "Test XP Comment", Where: ifd0, Category: core.CatContent, Risk: core.RiskMed},
		{Field: "Document name", Value: "Test Document", Where: ifd0, Category: core.CatContent, Risk: core.RiskMed},
		{Field: "XP title", Value: "Test XP Title", Where: ifd0, Category: core.CatContent, Risk: core.RiskLow},
		{Field: "XP keywords", Value: "Test XP Keywords", Where: ifd0, Category: core.CatContent, Risk: core.RiskLow},
		{Field: "XP subject", Value: "Test XP Subject", Where: ifd0, Category: core.CatContent, Risk: core.RiskLow},

		{Field: "Owner name", Value: "Test Owner", Where: sub, Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Body serial number", Value: "TESTBODY123", Where: sub, Category: core.CatDevice, Risk: core.RiskHigh},
		{Field: "Lens serial number", Value: "TESTLENS456", Where: sub, Category: core.CatDevice, Risk: core.RiskHigh},
		{Field: "MakerNote", Value: "32 bytes", Where: sub, Category: core.CatDevice, Risk: core.RiskHigh},
		{Field: "Lens make", Value: "Test Lens Make", Where: sub, Category: core.CatDevice, Risk: core.RiskLow},
		{Field: "Lens model", Value: "Test Lens Model", Where: sub, Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Date taken", Value: "2001:02:03 04:05:07", Where: sub, Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Date digitized", Value: "2001:02:03 04:05:08", Where: sub, Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Time zone", Value: "+01:00", Where: sub, Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "Time zone taken", Value: "+02:00", Where: sub, Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "Time zone digitized", Value: "+03:00", Where: sub, Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "User comment", Value: "Test Comment", Where: sub, Category: core.CatContent, Risk: core.RiskMed},
		{Field: "Image unique ID", Value: "0123456789abcdef0123456789abcdef", Where: sub, Category: core.CatHidden, Risk: core.RiskMed},

		{Field: "Embedded thumbnail", Value: "JPEG, 25 bytes", Where: ifd1, Category: core.CatThumbnail, Risk: core.RiskHigh},

		// XResolution in IFD0, Compression in IFD1 and the one Interop tag.
		{Field: "Other EXIF tags", Value: "3 tags", Where: top, Category: core.CatTechnical, Risk: core.RiskLow},
	}
	for _, order := range bothOrders {
		t.Run(order.String(), func(t *testing.T) {
			d := mustParse(t, coretest.FakeIFD(0).Encode(order))
			got := d.Findings(top, core.Options{})
			checkFindings(t, got)
			for _, w := range want {
				w.Action = core.ActionRemove
				f, ok := findField(got, w.Field)
				if !ok {
					t.Errorf("no finding for %q", w.Field)
				} else if f != w {
					t.Errorf("%s:\n got %+v\nwant %+v", w.Field, f, w)
				}
			}
			if len(got) != len(want) {
				t.Errorf("got %d findings, want %d: %q", len(got), len(want), fieldNames(got))
			}
		})
	}
}

// TestFakeEXIFFieldsAreAccurate keeps the list handlers assert against honest.
func TestFakeEXIFFieldsAreAccurate(t *testing.T) {
	findings := mustParse(t, coretest.FakeEXIF(0)).Findings("", core.Options{})
	names := fieldNames(findings)
	for _, field := range coretest.FakeEXIFFields() {
		if !slices.Contains(names, field) {
			t.Errorf("FakeEXIFFields lists %q, which Findings does not report", field)
		}
	}
}

func TestEveryNamedTagIsReported(t *testing.T) {
	const text = "Test Value"
	placements := []struct {
		name  string
		where string
		build func(coretest.Entry) *coretest.IFD
	}{
		{"IFD0", "W/IFD0", func(e coretest.Entry) *coretest.IFD {
			return &coretest.IFD{Entries: []coretest.Entry{e}}
		}},
		{"Exif", "W/Exif", func(e coretest.Entry) *coretest.IFD {
			return &coretest.IFD{Exif: &coretest.IFD{Entries: []coretest.Entry{e}}}
		}},
		{"IFD1", "W/IFD1", func(e coretest.Entry) *coretest.IFD {
			return &coretest.IFD{Next: &coretest.IFD{Entries: []coretest.Entry{e}}}
		}},
	}
	for _, order := range bothOrders {
		for _, spec := range namedTags {
			for _, place := range placements {
				t.Run(order.String()+"/"+spec.field+"/"+place.name, func(t *testing.T) {
					value := []byte(text + "\x00")
					want := text
					switch {
					case strings.HasPrefix(spec.field, "XP"):
						value = utf16Bytes(binary.LittleEndian, text)
					case spec.field == "User comment":
						value = append([]byte("ASCII\x00\x00\x00"), text...)
					case spec.field == "MakerNote" || strings.HasPrefix(spec.field, "Embedded"):
						want = "11 bytes"
					}
					entry := coretest.Entry{Tag: spec.tag, Type: coretest.TypeUndefined, Value: value}
					got := mustParse(t, place.build(entry).Encode(order)).Findings("W", core.Options{})
					wantFinding := core.Finding{
						Category: spec.category, Risk: spec.risk, Field: spec.field, Value: want,
						Where: place.where, Action: core.ActionRemove,
					}
					if len(got) != 1 || got[0] != wantFinding {
						t.Errorf("got %+v, want %+v", got, wantFinding)
					}
				})
			}
		}
	}
}

func TestNamedTagsAreUnique(t *testing.T) {
	seen := make(map[uint16]bool)
	for _, spec := range namedTags {
		if seen[spec.tag] {
			t.Errorf("tag %#04x is listed twice, which would make the catch-all count wrong", spec.tag)
		}
		seen[spec.tag] = true
		switch spec.tag {
		case tagOrientation, tagExifIFD, tagGPSIFD, tagInteropIFD, tagStripOffsets, tagStripByteCounts, tagJPEGOffset, tagJPEGLength:
			t.Errorf("tag %#04x is also handled specially", spec.tag)
		}
	}
}

func TestGPSCoordinates(t *testing.T) {
	type dms = [][2]uint32
	eiffelLat, eiffelLon := dms{{48, 1}, {51, 1}, {3024, 100}}, dms{{2, 1}, {17, 1}, {402, 10}}
	sydneyLat, sydneyLon := dms{{33, 1}, {51, 1}, {2412, 100}}, dms{{151, 1}, {12, 1}, {5472, 100}}
	libertyLat, libertyLon := dms{{40, 1}, {41, 1}, {2124, 100}}, dms{{74, 1}, {2, 1}, {402, 10}}
	rioLat, rioLon := dms{{22, 1}, {57, 1}, {684, 100}}, dms{{43, 1}, {12, 1}, {378, 10}}

	tests := []struct {
		name             string
		latRef, lonRef   any
		lat, lon         dms
		want             string
		latType, lonType uint16
	}{
		{name: "north east", latRef: "N", lonRef: "E", lat: eiffelLat, lon: eiffelLon, want: "48.858400, 2.294500"},
		{name: "south east", latRef: "S", lonRef: "E", lat: sydneyLat, lon: sydneyLon, want: "-33.856700, 151.215200"},
		{name: "north west", latRef: "N", lonRef: "W", lat: libertyLat, lon: libertyLon, want: "40.689233, -74.044500"},
		{name: "south west", latRef: "S", lonRef: "W", lat: rioLat, lon: rioLon, want: "-22.951900, -43.210500"},
		{name: "lower case references", latRef: "s", lonRef: "w", lat: rioLat, lon: rioLon, want: "-22.951900, -43.210500"},
		{name: "full word references", latRef: "South", lonRef: "West", lat: rioLat, lon: rioLon, want: "-22.951900, -43.210500"},
		{name: "references padded with spaces", latRef: " S ", lonRef: " W ", lat: rioLat, lon: rioLon, want: "-22.951900, -43.210500"},
		{name: "references stored as undefined bytes", latRef: []byte("S\x00"), lonRef: []byte("W\x00"), lat: rioLat, lon: rioLon, want: "-22.951900, -43.210500"},
		{name: "no references reads as north and east", lat: rioLat, lon: rioLon, want: "22.951900, 43.210500"},
		{name: "reference in an unusable type reads as north and east", latRef: uint16(83), lonRef: uint16(87), lat: rioLat, lon: rioLon, want: "22.951900, 43.210500"},
		{name: "decimal minutes with seconds of 0/0", latRef: "N", lonRef: "W",
			lat: dms{{48, 1}, {51504, 1000}, {0, 0}}, lon: dms{{2, 1}, {1767, 100}, {0, 0}}, want: "48.858400, -2.294500"},
		{name: "decimal degrees only", latRef: "S", lonRef: "W", lat: dms{{488584, 10000}}, lon: dms{{22945, 10000}}, want: "-48.858400, -2.294500"},
		{name: "degrees with minutes of 0/0 and real seconds", latRef: "N", lonRef: "E",
			lat: dms{{48, 1}, {0, 0}, {3600, 1}}, lon: dms{{2, 1}, {0, 1}, {0, 1}}, want: "49.000000, 2.000000"},
		{name: "south of zero is not negative zero", latRef: "S", lonRef: "W", lat: dms{{0, 1}, {0, 1}, {0, 1}}, lon: dms{{0, 1}, {0, 1}, {0, 1}}, want: "0.000000, 0.000000"},
		{name: "extra elements are ignored", latRef: "N", lonRef: "E",
			lat: dms{{48, 1}, {51, 1}, {3024, 100}, {99, 1}}, lon: dms{{2, 1}, {17, 1}, {402, 10}, {99, 1}}, want: "48.858400, 2.294500"},
		{name: "degrees of 0/0 cannot be decoded", latRef: "N", lonRef: "E", lat: dms{{0, 0}, {51, 1}, {1, 1}}, lon: dms{{0, 0}, {17, 1}, {1, 1}}, want: undecodable},
		{name: "latitude that does not decode names the longitude", latRef: "N", lonRef: "W", lat: dms{{0, 0}}, lon: libertyLon, want: "longitude -74.044500"},
		{name: "longitude that does not decode names the latitude", latRef: "S", lonRef: "E", lat: sydneyLat, lon: dms{{0, 0}}, want: "latitude -33.856700"},
		{name: "signed rationals with a negative degree", lat: dms{{uint32(0xFFFFFFDF), 1}, {51, 1}, {2412, 100}}, lon: dms{{uint32(0xFFFFFFB6), 1}, {2, 1}, {402, 10}},
			latType: coretest.TypeSRational, lonType: coretest.TypeSRational, want: "-33.856700, -74.044500"},
		{name: "a negative degree and a southern reference are not negated twice", latRef: "S", lonRef: "W",
			lat: dms{{uint32(0xFFFFFFDF), 1}, {51, 1}, {2412, 100}}, lon: dms{{uint32(0xFFFFFFB6), 1}, {2, 1}, {402, 10}},
			latType: coretest.TypeSRational, lonType: coretest.TypeSRational, want: "-33.856700, -74.044500"},
		{name: "largest rational does not overflow", latRef: "S", lonRef: "E", lat: dms{{0xFFFFFFFF, 1}}, lon: dms{{1, 0xFFFFFFFF}}, want: "-4294967295.000000, 0.000000"},
	}
	for _, order := range bothOrders {
		for _, tt := range tests {
			t.Run(order.String()+"/"+tt.name, func(t *testing.T) {
				var entries []coretest.Entry
				if tt.latRef != nil {
					entries = append(entries, coretest.Entry{Tag: gpsLatitudeRef, Value: tt.latRef})
				}
				entries = append(entries, coretest.Entry{Tag: gpsLatitude, Type: tt.latType, Value: tt.lat})
				if tt.lonRef != nil {
					entries = append(entries, coretest.Entry{Tag: gpsLongitudeRef, Value: tt.lonRef})
				}
				entries = append(entries, coretest.Entry{Tag: gpsLongitude, Type: tt.lonType, Value: tt.lon})
				findings := mustParse(t, gpsBlock(order, entries...)).Findings("W", core.Options{})
				f, ok := findField(findings, "GPS coordinates")
				if !ok {
					t.Fatalf("no coordinates in %q", fieldNames(findings))
				}
				if f.Value != tt.want {
					t.Errorf("coordinates = %q, want %q", f.Value, tt.want)
				}
				if f.Category != core.CatLocation || f.Risk != core.RiskHigh || f.Action != core.ActionRemove || f.Where != "W/GPS" {
					t.Errorf("finding = %+v", f)
				}
				if _, ok := findField(findings, "Other GPS tags"); ok {
					t.Errorf("references and coordinates fell into the catch-all: %q", fieldNames(findings))
				}
			})
		}
	}
}

func TestGPSPartialCoordinates(t *testing.T) {
	lat := coretest.Entry{Tag: gpsLatitude, Value: [][2]uint32{{48, 1}, {51, 1}, {3024, 100}}}
	lon := coretest.Entry{Tag: gpsLongitude, Value: [][2]uint32{{2, 1}, {17, 1}, {402, 10}}}
	tests := []struct {
		name    string
		entries []coretest.Entry
		field   string
		want    string
	}{
		{"latitude alone", []coretest.Entry{{Tag: gpsLatitudeRef, Value: "S"}, lat}, "GPS coordinates", "latitude -48.858400"},
		{"longitude alone", []coretest.Entry{{Tag: gpsLongitudeRef, Value: "W"}, lon}, "GPS coordinates", "longitude -2.294500"},
		{"coordinates in the wrong type", []coretest.Entry{{Tag: gpsLatitude, Value: uint32(48)}, {Tag: gpsLongitude, Value: uint32(2)}}, "GPS coordinates", undecodable},
		{"references with no coordinates are only counted", []coretest.Entry{{Tag: gpsLatitudeRef, Value: "N"}, {Tag: gpsLongitudeRef, Value: "E"}}, "Other GPS tags", "2 tags"},
		{"altitude above sea level", []coretest.Entry{{Tag: gpsAltitudeRef, Type: coretest.TypeByte, Value: []byte{0}}, {Tag: gpsAltitude, Value: [][2]uint32{{3305, 10}}}}, "GPS altitude", "330.5 m"},
		{"altitude below sea level", []coretest.Entry{{Tag: gpsAltitudeRef, Type: coretest.TypeByte, Value: []byte{1}}, {Tag: gpsAltitude, Value: [][2]uint32{{430, 1}}}}, "GPS altitude", "-430.0 m"},
		{"altitude reference stored as undefined", []coretest.Entry{{Tag: gpsAltitudeRef, Value: []byte{1}}, {Tag: gpsAltitude, Value: [][2]uint32{{430, 1}}}}, "GPS altitude", "-430.0 m"},
		{"altitude with no reference", []coretest.Entry{{Tag: gpsAltitude, Value: [][2]uint32{{12, 1}}}}, "GPS altitude", "12.0 m"},
		{"altitude of 0/0", []coretest.Entry{{Tag: gpsAltitude, Value: [][2]uint32{{0, 0}}}}, "GPS altitude", undecodable},
		{"timestamp with fractional seconds", []coretest.Entry{{Tag: gpsTimeStamp, Value: [][2]uint32{{23, 1}, {59, 1}, {5999, 100}}}}, "GPS timestamp", "23:59:59 UTC"},
		{"timestamp with an unknown part", []coretest.Entry{{Tag: gpsTimeStamp, Value: [][2]uint32{{23, 1}, {0, 0}, {1, 1}}}}, "GPS timestamp", undecodable},
		{"timestamp too short", []coretest.Entry{{Tag: gpsTimeStamp, Value: [][2]uint32{{23, 1}}}}, "GPS timestamp", undecodable},
		{"date", []coretest.Entry{{Tag: gpsDateStamp, Value: "2001:02:03"}}, "GPS date", "2001:02:03"},
		{"blank date is still reported", []coretest.Entry{{Tag: gpsDateStamp, Value: "    "}}, "GPS date", undecodable},
		{"date in the wrong type is still reported", []coretest.Entry{{Tag: gpsDateStamp, Value: uint32(1)}}, "GPS date", undecodable},
		{"unlisted tags are counted", []coretest.Entry{{Tag: 0x001B, Value: []byte("GPS method")}, {Tag: 0x0014, Value: [][2]uint32{{1, 1}}}}, "Other GPS tags", "2 tags"},
	}
	for _, order := range bothOrders {
		for _, tt := range tests {
			t.Run(order.String()+"/"+tt.name, func(t *testing.T) {
				findings := mustParse(t, gpsBlock(order, tt.entries...)).Findings("W", core.Options{})
				f, ok := findField(findings, tt.field)
				if !ok {
					t.Fatalf("no %q in %q", tt.field, fieldNames(findings))
				}
				if f.Value != tt.want {
					t.Errorf("value = %q, want %q", f.Value, tt.want)
				}
			})
		}
	}
}

func TestOrientationFindings(t *testing.T) {
	for _, order := range bothOrders {
		for o := uint16(0); o <= 10; o++ {
			valid := o >= 1 && o <= 8
			block := (&coretest.IFD{Entries: []coretest.Entry{{Tag: tagOrientation, Value: o}}}).Encode(order)
			d := mustParse(t, block)

			wantValue := uint16(0)
			if valid {
				wantValue = o
			}
			if got := d.Orientation(); got != wantValue {
				t.Errorf("%v: Orientation() of %d = %d", order, o, got)
			}

			f, ok := findField(d.Findings("W", core.Options{}), "Orientation")
			if !ok {
				t.Fatalf("%v: orientation %d not reported", order, o)
			}
			if f.Category != core.CatTechnical || f.Risk != core.RiskNone || f.Where != "W/IFD0" {
				t.Errorf("%v: finding = %+v", order, f)
			}
			if o > 1 && valid {
				if f.Action != core.ActionRetain || f.Reason == "" {
					t.Errorf("%v: orientation %d: action %q reason %q, want retained with a reason", order, o, f.Action, f.Reason)
				}
				if got := d.Replacement(core.Options{}); string(got) != string(Minimal(o)) {
					t.Errorf("%v: Replacement = %x", order, got)
				}
			} else {
				if f.Action != core.ActionRemove || f.Reason != "" {
					t.Errorf("%v: orientation %d: action %q reason %q, want removed", order, o, f.Action, f.Reason)
				}
				if got := d.Replacement(core.Options{}); got != nil {
					t.Errorf("%v: orientation %d: Replacement = %x, want nil", order, o, got)
				}
			}
			if !valid && !strings.Contains(f.Value, "invalid") {
				t.Errorf("%v: orientation %d has value %q", order, o, f.Value)
			}

			strict, _ := findField(d.Findings("W", core.Options{Strict: true}), "Orientation")
			if strict.Action != core.ActionRemove || strict.Reason != "" {
				t.Errorf("%v: strict orientation %d: %+v", order, o, strict)
			}
			if d.Replacement(core.Options{Strict: true}) != nil {
				t.Errorf("%v: strict mode offered a replacement", order)
			}
		}
	}
}

func TestOrientationTagShapes(t *testing.T) {
	tests := []struct {
		name  string
		entry coretest.Entry
		want  uint16
	}{
		{"LONG is tolerated", coretest.Entry{Tag: tagOrientation, Value: uint32(6)}, 6},
		{"large LONG does not wrap into range", coretest.Entry{Tag: tagOrientation, Value: uint32(0x10006)}, 0},
		{"array of two", coretest.Entry{Tag: tagOrientation, Value: []uint16{6, 6}}, 0},
		{"text", coretest.Entry{Tag: tagOrientation, Value: "6"}, 0},
		{"undefined", coretest.Entry{Tag: tagOrientation, Value: []byte{0, 6}}, 0},
		{"unknown type", coretest.Entry{Tag: tagOrientation, Type: 99, Value: uint16(6)}, 0},
	}
	for _, order := range bothOrders {
		for _, tt := range tests {
			t.Run(order.String()+"/"+tt.name, func(t *testing.T) {
				d := mustParse(t, (&coretest.IFD{Entries: []coretest.Entry{tt.entry}}).Encode(order))
				if got := d.Orientation(); got != tt.want {
					t.Errorf("Orientation() = %d, want %d", got, tt.want)
				}
				f, ok := findField(d.Findings("W", core.Options{}), "Orientation")
				if !ok {
					t.Fatal("not reported")
				}
				if wantRetain := tt.want > 1; (f.Action == core.ActionRetain) != wantRetain {
					t.Errorf("action = %q", f.Action)
				}
			})
		}
	}
}

// TestOrientationOutsideIFD0IsIgnored matters because IFD1 commonly has its own
// orientation for the thumbnail, which says nothing about the image.
func TestOrientationOutsideIFD0IsIgnored(t *testing.T) {
	tag := coretest.Entry{Tag: tagOrientation, Value: uint16(6)}
	blocks := map[string]*coretest.IFD{
		"IFD1": {Next: &coretest.IFD{Entries: []coretest.Entry{tag}}},
		"Exif": {Exif: &coretest.IFD{Entries: []coretest.Entry{tag}}},
	}
	for name, ifd := range blocks {
		t.Run(name, func(t *testing.T) {
			d := mustParse(t, ifd.Encode(binary.LittleEndian))
			if d.Orientation() != 0 || d.Replacement(core.Options{}) != nil {
				t.Errorf("Orientation = %d", d.Orientation())
			}
			findings := d.Findings("W", core.Options{})
			if _, ok := findField(findings, "Orientation"); ok {
				t.Errorf("reported as the image's orientation: %q", fieldNames(findings))
			}
			if f, _ := findField(findings, "Other EXIF tags"); f.Value != "1 tag" {
				t.Errorf("catch-all = %q", f.Value)
			}
		})
	}
}

func TestDuplicateTagsUseTheFirstAndCountTheRest(t *testing.T) {
	block := (&coretest.IFD{Entries: []coretest.Entry{
		{Tag: 0x013B, Value: "First Author"},
		{Tag: 0x013B, Value: "Second Author"},
		{Tag: tagOrientation, Value: uint16(6)},
		{Tag: tagOrientation, Value: uint16(3)},
	}}).Encode(binary.LittleEndian)
	d := mustParse(t, block)
	if got := d.Orientation(); got != 6 {
		t.Errorf("Orientation = %d, want the first value", got)
	}
	findings := d.Findings("W", core.Options{})
	if f, _ := findField(findings, "Artist"); f.Value != "First Author" {
		t.Errorf("Artist = %q", f.Value)
	}
	if f, _ := findField(findings, "Other EXIF tags"); f.Value != "2 tags" {
		t.Errorf("catch-all = %q", f.Value)
	}
}

func TestUnusableNamedTagsAreCounted(t *testing.T) {
	tests := []struct {
		name  string
		entry coretest.Entry
	}{
		{"blank text, as cameras leave unused fields", coretest.Entry{Tag: 0x013B, Value: "        "}},
		{"empty text", coretest.Entry{Tag: 0x013B, Value: ""}},
		{"only NUL", coretest.Entry{Tag: 0x013B, Value: []byte{0, 0, 0, 0, 0, 0}}},
		{"text tag holding a number", coretest.Entry{Tag: 0x013B, Value: uint16(7)}},
		{"text tag of an unknown type", coretest.Entry{Tag: 0x013B, Type: 99, Value: "Test Author"}},
		{"XP tag holding a number", coretest.Entry{Tag: 0x9C9D, Value: uint32(7)}},
		{"user comment holding a number", coretest.Entry{Tag: 0x9286, Value: uint32(7)}},
		{"blank user comment", coretest.Entry{Tag: 0x9286, Value: []byte("\x00\x00\x00\x00\x00\x00\x00\x00          ")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustParse(t, (&coretest.IFD{Entries: []coretest.Entry{tt.entry}}).Encode(binary.LittleEndian))
			findings := d.Findings("W", core.Options{})
			if len(findings) != 1 || findings[0].Field != "Other EXIF tags" || findings[0].Value != "1 tag" {
				t.Errorf("findings = %+v", findings)
			}
		})
	}
}

func TestThumbnailReporting(t *testing.T) {
	const (
		jpegOffset  = uint16(tagJPEGOffset)
		jpegLength  = uint16(tagJPEGLength)
		stripOffset = uint16(tagStripOffsets)
		stripBytes  = uint16(tagStripByteCounts)
	)
	tests := []struct {
		name      string
		entries   []coretest.Entry
		want      string
		wantOther string
	}{
		{"JPEG", []coretest.Entry{{Tag: jpegOffset, Value: uint32(100)}, {Tag: jpegLength, Value: uint32(5000)}}, "JPEG, 5000 bytes", ""},
		{"JPEG with a SHORT length", []coretest.Entry{{Tag: jpegOffset, Value: uint32(100)}, {Tag: jpegLength, Value: uint16(500)}}, "JPEG, 500 bytes", ""},
		{"JPEG with no length", []coretest.Entry{{Tag: jpegOffset, Value: uint32(100)}}, "JPEG, size not given", ""},
		{"uncompressed strips of SHORTs", []coretest.Entry{{Tag: stripOffset, Value: []uint32{10, 20}}, {Tag: stripBytes, Value: []uint16{100, 200}}}, "uncompressed, 300 bytes", ""},
		{"uncompressed strips of LONGs", []coretest.Entry{{Tag: stripOffset, Value: []uint32{10, 20, 30}}, {Tag: stripBytes, Value: []uint32{100, 200, 300}}}, "uncompressed, 600 bytes", ""},
		{"uncompressed with no byte counts", []coretest.Entry{{Tag: stripOffset, Value: uint32(10)}}, "uncompressed, size not given", ""},
		{"JPEG tags win over strip tags", []coretest.Entry{{Tag: stripOffset, Value: uint32(10)}, {Tag: stripBytes, Value: uint32(9)}, {Tag: jpegOffset, Value: uint32(100)}, {Tag: jpegLength, Value: uint32(5000)}}, "JPEG, 5000 bytes", "2 tags"},
		{"a length of the wrong type reads as zero", []coretest.Entry{{Tag: jpegOffset, Value: uint32(100)}, {Tag: jpegLength, Value: "x"}}, "JPEG, 0 bytes", ""},
		{"a length alone is not a thumbnail", []coretest.Entry{{Tag: jpegLength, Value: uint32(100)}}, "", "1 tag"},
	}
	for _, order := range bothOrders {
		for _, tt := range tests {
			t.Run(order.String()+"/"+tt.name, func(t *testing.T) {
				block := (&coretest.IFD{Next: &coretest.IFD{Entries: tt.entries}}).Encode(order)
				findings := mustParse(t, block).Findings("W", core.Options{})
				f, ok := findField(findings, "Embedded thumbnail")
				switch {
				case tt.want == "" && ok:
					t.Errorf("unexpected thumbnail: %+v", f)
				case tt.want != "" && !ok:
					t.Fatalf("no thumbnail in %q", fieldNames(findings))
				case ok:
					if f.Value != tt.want || f.Risk != core.RiskHigh || f.Category != core.CatThumbnail || f.Action != core.ActionRemove || f.Where != "W/IFD1" {
						t.Errorf("thumbnail = %+v, want value %q", f, tt.want)
					}
				}
				other, hasOther := findField(findings, "Other EXIF tags")
				if hasOther != (tt.wantOther != "") || other.Value != tt.wantOther {
					t.Errorf("catch-all = %q, want %q", other.Value, tt.wantOther)
				}
			})
		}
	}
}

// TestStripTagsInIFD0AreNotAThumbnail keeps a full TIFF's main image from being
// reported as a preview.
func TestStripTagsInIFD0AreNotAThumbnail(t *testing.T) {
	block := (&coretest.IFD{Entries: []coretest.Entry{
		{Tag: tagStripOffsets, Value: uint32(10)},
		{Tag: tagStripByteCounts, Value: uint32(100)},
	}}).Encode(binary.LittleEndian)
	findings := mustParse(t, block).Findings("W", core.Options{})
	if _, ok := findField(findings, "Embedded thumbnail"); ok {
		t.Errorf("reported a thumbnail: %+v", findings)
	}
}

func TestThumbnailOverrunningTheBlockIsNotDamage(t *testing.T) {
	block := (&coretest.IFD{Next: &coretest.IFD{Entries: []coretest.Entry{
		{Tag: tagJPEGOffset, Value: uint32(0xFFFFFF00)},
		{Tag: tagJPEGLength, Value: uint32(0xFFFFFF)},
	}}}).Encode(binary.BigEndian)
	findings := mustParse(t, block).Findings("W", core.Options{})
	if f, _ := findField(findings, "Embedded thumbnail"); f.Value != "JPEG, 16777215 bytes" {
		t.Errorf("thumbnail = %q", f.Value)
	}
}

func TestMakerNoteDeclaredSize(t *testing.T) {
	block := (&coretest.IFD{Exif: &coretest.IFD{Entries: []coretest.Entry{
		{Tag: 0x927C, Type: 99, Count: 1 << 31, Value: []byte{1, 2, 3, 4, 5}},
	}}}).Encode(binary.LittleEndian)
	f, ok := findField(mustParse(t, block).Findings("W", core.Options{}), "MakerNote")
	if !ok || f.Value != "2147483648 bytes" || f.Risk != core.RiskHigh {
		t.Errorf("MakerNote = %+v", f)
	}
}

func TestInteropTagsAreCountedNotNamed(t *testing.T) {
	// These numbers mean something else in IFD0 and must not be read through
	// that table, where 0x0002 or 0x013B could be mistaken for real fields.
	block := (&coretest.IFD{Exif: &coretest.IFD{Interop: &coretest.IFD{Entries: []coretest.Entry{
		{Tag: 0x0001, Value: "R98"},
		{Tag: 0x013B, Value: "Not An Artist"},
	}}}}).Encode(binary.LittleEndian)
	findings := mustParse(t, block).Findings("W", core.Options{})
	if len(findings) != 1 || findings[0].Field != "Other EXIF tags" || findings[0].Value != "2 tags" {
		t.Errorf("findings = %+v", findings)
	}
}

func TestGPSTagsAreNotReadAsImageTags(t *testing.T) {
	// 0x0110 is Camera model in IFD0 but means nothing in a GPS IFD.
	block := gpsBlock(binary.LittleEndian, coretest.Entry{Tag: 0x0110, Value: "Not A Model"})
	findings := mustParse(t, block).Findings("W", core.Options{})
	if _, ok := findField(findings, "Camera model"); ok {
		t.Errorf("findings = %+v", findings)
	}
	if f, _ := findField(findings, "Other GPS tags"); f.Value != "1 tag" {
		t.Errorf("catch-all = %q", f.Value)
	}
}

func TestWhereIsJoinedAndSanitised(t *testing.T) {
	block := (&coretest.IFD{Entries: []coretest.Entry{{Tag: 0x013B, Value: "x"}, {Tag: 0x011A, Value: uint32(1)}}}).Encode(binary.LittleEndian)
	d := mustParse(t, block)
	if f, _ := findField(d.Findings("", core.Options{}), "Artist"); f.Where != "IFD0" {
		t.Errorf("empty where: %q", f.Where)
	}
	if f, _ := findField(d.Findings("", core.Options{}), "Other EXIF tags"); f.Where != "" {
		t.Errorf("empty where on the catch-all: %q", f.Where)
	}
	if f, _ := findField(d.Findings("evil\x1b[2J", core.Options{}), "Artist"); f.Where != "evil[2J/IFD0" {
		t.Errorf("hostile where: %q", f.Where)
	}
}

func TestValuesAreSanitised(t *testing.T) {
	block := (&coretest.IFD{Entries: []coretest.Entry{
		{Tag: 0x013B, Value: "\x1b[31mEvil\u202Etxt.exe\nGPS: none"},
		{Tag: 0x9C9D, Type: coretest.TypeByte, Value: utf16Bytes(binary.LittleEndian, "\x1b]0;owned\x07\u202Eauthor")},
		{Tag: 0x8298, Value: strings.Repeat("A", 100000)},
	}}).Encode(binary.LittleEndian)
	findings := mustParse(t, block).Findings("W", core.Options{})
	checkFindings(t, findings)
	if f, _ := findField(findings, "Artist"); f.Value != "[31mEviltxt.exe GPS: none" {
		t.Errorf("Artist = %q", f.Value)
	}
	if f, _ := findField(findings, "XP author"); f.Value != "]0;ownedauthor" {
		t.Errorf("XP author = %q", f.Value)
	}
	f, _ := findField(findings, "Copyright")
	if !strings.HasSuffix(f.Value, "...") || utf8.RuneCountInString(f.Value) != 203 {
		t.Errorf("long value was not truncated: %d characters", len(f.Value))
	}
}

func TestBlocksThatLookEmptyStillReportThemselves(t *testing.T) {
	le := binary.LittleEndian
	onlyOrientationLE := (&coretest.IFD{Entries: []coretest.Entry{{Tag: tagOrientation, Value: uint16(6)}}}).Encode(le)
	tests := []struct {
		name  string
		block []byte
	}{
		{"no entries", appendIFD(rawHeader(le, headerSize), le, 0)},
		{"only an orientation, in the other byte order", onlyOrientationLE},
		{"the minimal block plus one hidden byte", append(Minimal(6), 'x')},
		{"the minimal block with secrets after it", append(Minimal(6), "Test Author"...)},
		{"the minimal block with an IFD offset moved to hide bytes", movedMinimal()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := mustParse(t, tt.block).Findings("W", core.Options{})
			f, ok := findField(findings, "EXIF block")
			if !ok {
				t.Fatalf("no block finding in %q", fieldNames(findings))
			}
			if f.Action != core.ActionRemove || f.Category != core.CatHidden {
				t.Errorf("finding = %+v", f)
			}
			if f.Value != formatSize(uint64(len(tt.block))) {
				t.Errorf("value = %q", f.Value)
			}
			checkBlock(t, tt.block)
		})
	}
}

// movedMinimal is a big-endian orientation-only block with secret bytes between
// the header and IFD0, where no tag refers to them.
func movedMinimal() []byte {
	be := binary.BigEndian
	const hidden = "SECRETSS"
	block := append(rawHeader(be, headerSize+uint32(len(hidden))), hidden...)
	return appendIFD(block, be, 0, rawEntry{tag: tagOrientation, typ: typeShort, count: 1, value: 6 << 16})
}

func TestTwoParsesReportTheSame(t *testing.T) {
	// Inspect and Clean each parse the block once, so any difference between two
	// parses would make them disagree.
	block := coretest.FakeEXIF(6)
	a := mustParse(t, block).Findings("W", core.Options{})
	b := mustParse(t, block).Findings("W", core.Options{})
	if !slices.Equal(a, b) {
		t.Error("two parses of one block reported differently")
	}
}
