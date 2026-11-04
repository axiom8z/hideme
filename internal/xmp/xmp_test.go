package xmp

import (
	"bytes"
	"encoding/binary"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

const namespaces = `xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
 xmlns:dc="http://purl.org/dc/elements/1.1/"
 xmlns:xmp="http://ns.adobe.com/xap/1.0/"
 xmlns:xmpMM="http://ns.adobe.com/xap/1.0/mm/"
 xmlns:stEvt="http://ns.adobe.com/xap/1.0/sType/ResourceEvent#"
 xmlns:exif="http://ns.adobe.com/exif/1.0/"
 xmlns:tiff="http://ns.adobe.com/tiff/1.0/"
 xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/"
 xmlns:Iptc4xmpCore="http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/"
 xmlns:custom="http://example.com/custom/"`

// description wraps properties in the structure real packets have: rdf:RDF
// holding an rdf:Description, with attrs on the description.
func description(attrs, body string) string {
	return `<rdf:RDF ` + namespaces + `><rdf:Description rdf:about="" ` + attrs + `>` + body + `</rdf:Description></rdf:RDF>`
}

func value(t *testing.T, findings []core.Finding, field string) string {
	t.Helper()
	i := slices.IndexFunc(findings, func(f core.Finding) bool { return f.Field == field })
	if i < 0 {
		t.Fatalf("no %q finding in %q", field, fieldNames(findings))
	}
	return findings[i].Value
}

func fieldNames(findings []core.Finding) []string {
	names := make([]string, len(findings))
	for i, f := range findings {
		names[i] = f.Field
	}
	return names
}

// checkPacket asserts what must hold for any bytes at all. It is shared by the
// truncation and corruption sweeps and by FuzzFindings.
func checkPacket(t *testing.T, packet []byte) {
	t.Helper()
	findings := Findings(packet, "W")
	if len(findings) == 0 {
		t.Fatalf("no findings for %q", packet)
	}
	checkFindings(t, findings)
	if !slices.Equal(findings, Findings(packet, "W")) {
		t.Error("Findings is not deterministic")
	}
}

func checkFindings(t *testing.T, findings []core.Finding) {
	t.Helper()
	for _, f := range findings {
		if f.Action != core.ActionRemove {
			t.Errorf("every packet is removed, but found %+v", f)
		}
		if f.Field == "" || f.Category == "" || f.Risk == "" {
			t.Errorf("incomplete finding: %+v", f)
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

func TestFakePacketReportsEverything(t *testing.T) {
	want := []core.Finding{
		{Field: "GPS coordinates", Value: coretest.EiffelTower, Where: "APP1/exif:GPS", Category: core.CatLocation, Risk: core.RiskHigh},
		{Field: "City", Value: "Test City", Where: "APP1/photoshop:City", Category: core.CatLocation, Risk: core.RiskHigh},
		{Field: "Location", Value: "Test Location", Where: "APP1/Iptc4xmpCore:Location", Category: core.CatLocation, Risk: core.RiskHigh},
		{Field: "State", Value: "Test State", Where: "APP1/photoshop:State", Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "Country", Value: "Test Country", Where: "APP1/photoshop:Country", Category: core.CatLocation, Risk: core.RiskMed},
		{Field: "Creator", Value: "Test Author", Where: "APP1/dc:creator", Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Rights", Value: "Test Rights", Where: "APP1/dc:rights", Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "PDF author", Value: "Test PDF Author", Where: "APP1/pdf:Author", Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Credit", Value: "Test Credit", Where: "APP1/photoshop:Credit", Category: core.CatIdentity, Risk: core.RiskHigh},
		{Field: "Author's position", Value: "Test Position", Where: "APP1/photoshop:AuthorsPosition", Category: core.CatIdentity, Risk: core.RiskMed},
		{Field: "Serial number", Value: "TESTSERIAL123", Where: "APP1/aux:SerialNumber", Category: core.CatDevice, Risk: core.RiskHigh},
		{Field: "Camera make", Value: "Test Make", Where: "APP1/tiff:Make", Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Camera model", Value: "Test Model", Where: "APP1/tiff:Model", Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Lens", Value: "Test Lens", Where: "APP1/aux:Lens", Category: core.CatDevice, Risk: core.RiskMed},
		{Field: "Create date", Value: "2001-02-03T04:05:06+01:00", Where: "APP1/xmp:CreateDate", Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Modify date", Value: "2001-02-03T04:05:07+01:00", Where: "APP1/xmp:ModifyDate", Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Metadata date", Value: "2001-02-03T04:05:08+01:00", Where: "APP1/xmp:MetadataDate", Category: core.CatTime, Risk: core.RiskMed},
		{Field: "Creator tool", Value: "Test Editor 1.0", Where: "APP1/xmp:CreatorTool", Category: core.CatSoftware, Risk: core.RiskLow},
		{Field: "PDF producer", Value: "Test Producer", Where: "APP1/pdf:Producer", Category: core.CatSoftware, Risk: core.RiskLow},
		{Field: "XMP toolkit", Value: "Test XMP Toolkit", Where: "APP1/x:xmptk", Category: core.CatSoftware, Risk: core.RiskLow},
		{Field: "Edit history", Value: "2 events", Where: "APP1/xmpMM:History", Category: core.CatHidden, Risk: core.RiskMed},
		{Field: "Document ID", Value: "xmp.did:test-document", Where: "APP1/xmpMM:DocumentID", Category: core.CatHidden, Risk: core.RiskMed},
		{Field: "Instance ID", Value: "xmp.iid:test-instance", Where: "APP1/xmpMM:InstanceID", Category: core.CatHidden, Risk: core.RiskMed},
		{Field: "Description", Value: "Test Description", Where: "APP1/dc:description", Category: core.CatContent, Risk: core.RiskMed},
		{Field: "Title", Value: "Test Title", Where: "APP1/dc:title", Category: core.CatContent, Risk: core.RiskLow},
		{Field: "Keywords", Value: "Test Keyword, Second Keyword", Where: "APP1/dc:subject", Category: core.CatContent, Risk: core.RiskLow},
		{Field: "Rights marked", Value: "True", Where: "APP1/xmpRights:Marked", Category: core.CatContent, Risk: core.RiskLow},
	}
	got := Findings(coretest.FakeXMP(), "APP1")
	checkFindings(t, got)
	for _, w := range want {
		w.Action = core.ActionRemove
		i := slices.IndexFunc(got, func(f core.Finding) bool { return f.Field == w.Field })
		if i < 0 {
			t.Errorf("no finding for %q", w.Field)
		} else if got[i] != w {
			t.Errorf("%s:\n got %+v\nwant %+v", w.Field, got[i], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d findings, want %d: %q", len(got), len(want), fieldNames(got))
	}
}

// TestFakeXMPFieldsAreAccurate keeps the list handlers assert against honest.
func TestFakeXMPFieldsAreAccurate(t *testing.T) {
	names := fieldNames(Findings(coretest.FakeXMP(), ""))
	for _, field := range coretest.FakeXMPFields() {
		if !slices.Contains(names, field) {
			t.Errorf("FakeXMPFields lists %q, which Findings does not report", field)
		}
	}
}

func TestPropertyTableIsConsistent(t *testing.T) {
	seen := make(map[string]bool)
	for _, p := range properties {
		if seen[p.name] {
			t.Errorf("%q is listed twice, so its first row would never be used", p.name)
		}
		seen[p.name] = true
		if p.field == "" || p.category == "" || p.risk == "" {
			t.Errorf("incomplete row: %+v", p)
		}
		prefix, _, ok := strings.Cut(p.name, ":")
		if !ok {
			t.Errorf("%q has no prefix", p.name)
		}
		if !slices.Contains(slices.Collect(maps.Values(canonicalPrefixes)), prefix) {
			t.Errorf("%q uses a prefix no namespace maps to, so a real packet could never match it", p.name)
		}
	}
}

// TestRequiredProperties walks the properties the design requires, each in both
// forms a writer can use.
func TestRequiredProperties(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"dc:creator", "Creator"}, {"dc:rights", "Rights"}, {"dc:title", "Title"},
		{"dc:description", "Description"}, {"dc:subject", "Keywords"},
		{"xmp:CreatorTool", "Creator tool"}, {"xmp:CreateDate", "Create date"},
		{"xmp:ModifyDate", "Modify date"}, {"xmp:MetadataDate", "Metadata date"},
		{"tiff:Make", "Camera make"}, {"tiff:Model", "Camera model"},
		{"aux:SerialNumber", "Serial number"}, {"aux:Lens", "Lens"},
		{"photoshop:City", "City"}, {"photoshop:State", "State"}, {"photoshop:Country", "Country"},
		{"photoshop:AuthorsPosition", "Author's position"}, {"photoshop:Credit", "Credit"},
		{"Iptc4xmpCore:Location", "Location"},
		{"xmpMM:DocumentID", "Document ID"}, {"xmpMM:InstanceID", "Instance ID"},
		{"pdf:Producer", "PDF producer"}, {"pdf:Author", "PDF author"},
		{"xmpRights:Marked", "Rights marked"},
	}
	allNamespaces := namespaces + ` xmlns:aux="http://ns.adobe.com/exif/1.0/aux/" xmlns:pdf="http://ns.adobe.com/pdf/1.3/" xmlns:xmpRights="http://ns.adobe.com/xap/1.0/rights/"`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attr := `<rdf:RDF ` + allNamespaces + `><rdf:Description ` + tt.name + `="Test Value"/></rdf:RDF>`
			if got := value(t, Findings([]byte(attr), "W"), tt.field); got != "Test Value" {
				t.Errorf("attribute form: %q", got)
			}
			element := `<rdf:RDF ` + allNamespaces + `><rdf:Description><` + tt.name + `>Test Value</` + tt.name + `></rdf:Description></rdf:RDF>`
			if got := value(t, Findings([]byte(element), "W"), tt.field); got != "Test Value" {
				t.Errorf("element form: %q", got)
			}
			if got := Findings([]byte(attr), "W"); len(got) != 1 {
				t.Errorf("attribute form gave %q", fieldNames(got))
			}
		})
	}
}

func TestPropertyForms(t *testing.T) {
	tests := []struct {
		name  string
		attrs string
		body  string
		field string
		want  string
	}{
		{name: "attribute", attrs: `dc:creator="Test Author"`, field: "Creator", want: "Test Author"},
		{name: "element", body: `<dc:creator>Test Author</dc:creator>`, field: "Creator", want: "Test Author"},
		{name: "ordered list", body: `<dc:creator><rdf:Seq><rdf:li>One</rdf:li><rdf:li>Two</rdf:li></rdf:Seq></dc:creator>`, field: "Creator", want: "One, Two"},
		{name: "unordered list", body: `<dc:subject><rdf:Bag><rdf:li>One</rdf:li><rdf:li>Two</rdf:li></rdf:Bag></dc:subject>`, field: "Keywords", want: "One, Two"},
		{name: "language alternatives", body: `<dc:title><rdf:Alt><rdf:li xml:lang="x-default">Hello</rdf:li><rdf:li xml:lang="fr">Bonjour</rdf:li></rdf:Alt></dc:title>`, field: "Title", want: "Hello, Bonjour"},
		{name: "list with whitespace between items", body: "<dc:subject>\n <rdf:Bag>\n  <rdf:li>One</rdf:li>\n  <rdf:li>Two</rdf:li>\n </rdf:Bag>\n</dc:subject>", field: "Keywords", want: "One, Two"},
		{name: "repeated values are shown once", body: `<dc:title><rdf:Alt><rdf:li xml:lang="x-default">Same</rdf:li><rdf:li xml:lang="fr">Same</rdf:li></rdf:Alt></dc:title>`, field: "Title", want: "Same"},
		{name: "empty items are skipped", body: `<dc:subject><rdf:Bag><rdf:li/><rdf:li></rdf:li><rdf:li>One</rdf:li></rdf:Bag></dc:subject>`, field: "Keywords", want: "One"},
		{name: "resource reference", body: `<dc:creator rdf:resource="http://example.com/me"/>`, field: "Creator", want: "http://example.com/me"},
		{name: "structure in a list item", body: `<dc:creator><rdf:Seq><rdf:li><rdf:Description><rdf:value>Nested</rdf:value></rdf:Description></rdf:li></rdf:Seq></dc:creator>`, field: "Creator", want: "Nested"},
		{name: "entities are decoded", attrs: `dc:creator="Tom &amp; Jerry &#233;"`, field: "Creator", want: "Tom & Jerry é"},
		{name: "element entities are decoded", body: `<dc:creator>Tom &amp; Jerry &#233;</dc:creator>`, field: "Creator", want: "Tom & Jerry é"},
		{name: "CDATA", body: `<dc:creator><![CDATA[Tom <b> & Jerry]]></dc:creator>`, field: "Creator", want: "Tom <b> & Jerry"},
		{name: "comments are not values", body: `<dc:creator><!-- not me -->Test Author</dc:creator>`, field: "Creator", want: "Test Author"},
		{name: "empty element is still reported", body: `<dc:creator/>`, field: "Creator", want: ""},
		{name: "property nested under an unknown structure", body: `<custom:wrapper><rdf:Description dc:creator="Deep"/></custom:wrapper>`, field: "Creator", want: "Deep"},
		{name: "element nested under an unknown structure", body: `<custom:wrapper><dc:creator>Deep</dc:creator></custom:wrapper>`, field: "Creator", want: "Deep"},
		{name: "other properties are counted", attrs: `custom:a="1" custom:b="2"`, body: `<custom:c>3</custom:c>`, field: "Other XMP properties", want: "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := []byte(description(tt.attrs, tt.body))
			if got := value(t, Findings(packet, "W"), tt.field); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
			}
			checkPacket(t, packet)
		})
	}
}

func TestCountingOtherProperties(t *testing.T) {
	packet := []byte(description(`custom:a="1" xml:lang="en"`, `<custom:b rdf:parseType="Resource"><rdf:value>x</rdf:value></custom:b>`))
	if got := value(t, Findings(packet, "W"), "Other XMP properties"); got != "2" {
		t.Errorf("Other XMP properties = %q, want the two custom names only", got)
	}
}

func TestEditHistory(t *testing.T) {
	event := func(n int) string {
		return strings.Repeat(`<rdf:li stEvt:action="saved" stEvt:softwareAgent="Test Editor"/>`, n)
	}
	tests := []struct {
		name string
		body string
		want string
	}{
		{"none", `<xmpMM:History/>`, ""},
		{"empty list", `<xmpMM:History><rdf:Seq/></xmpMM:History>`, ""},
		{"one", `<xmpMM:History><rdf:Seq>` + event(1) + `</rdf:Seq></xmpMM:History>`, "1 event"},
		{"several", `<xmpMM:History><rdf:Seq>` + event(5) + `</rdf:Seq></xmpMM:History>`, "5 events"},
		{"more than values are kept for", `<xmpMM:History><rdf:Seq>` + event(1000) + `</rdf:Seq></xmpMM:History>`, "1000 events"},
		{"events with their own lists are counted once", `<xmpMM:History><rdf:Seq><rdf:li><rdf:Description><stEvt:changed><rdf:Bag><rdf:li>x</rdf:li><rdf:li>y</rdf:li></rdf:Bag></stEvt:changed></rdf:Description></rdf:li></rdf:Seq></xmpMM:History>`, "1 event"},
		{"list items in resource form", `<xmpMM:History><rdf:Seq><rdf:li rdf:parseType="Resource"><stEvt:action>saved</stEvt:action></rdf:li><rdf:li rdf:parseType="Resource"><stEvt:action>saved</stEvt:action></rdf:li></rdf:Seq></xmpMM:History>`, "2 events"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Findings([]byte(description("", tt.body)), "W")
			if got := value(t, findings, "Edit history"); got != tt.want {
				t.Errorf("Edit history = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("attribute form shows the value", func(t *testing.T) {
		packet := description(`xmpMM:History="saved twice"`, "")
		if got := value(t, Findings([]byte(packet), "W"), "Edit history"); got != "saved twice" {
			t.Errorf("Edit history = %q", got)
		}
	})
}

func TestGPSCoordinatesInPackets(t *testing.T) {
	tests := []struct {
		name string
		lat  string
		lon  string
		want string
	}{
		{"decimal minutes, north east", "48,51.5N", "2,17.67E", "48.858333, 2.294500"},
		{"decimal minutes, south west", "33,51.402S", "74,2.67W", "-33.856700, -74.044500"},
		{"degrees minutes seconds", "48,51,30N", "2,17,40.2E", "48.858333, 2.294500"},
		{"degrees minutes seconds, south west", "22,57,6.84S", "43,12,37.8W", "-22.951900, -43.210500"},
		{"lower case letters", "48,51.5s", "2,17.67w", "-48.858333, -2.294500"},
		{"bare decimal degrees", "48.8584", "-2.2945", "48.858400, -2.294500"},
		{"only a latitude", "48,51.5N", "", "latitude 48.858333"},
		{"only a longitude", "", "2,17.67W", "longitude -2.294500"},
		{"something else is shown as written", "near the tower", "2,17.67E", "near the tower, 2.294500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs, body := "", ""
			if tt.lat != "" {
				attrs += ` exif:GPSLatitude="` + tt.lat + `"`
				body += `<exif:GPSLatitude>` + tt.lat + `</exif:GPSLatitude>`
			}
			if tt.lon != "" {
				attrs += ` exif:GPSLongitude="` + tt.lon + `"`
				body += `<exif:GPSLongitude>` + tt.lon + `</exif:GPSLongitude>`
			}
			for name, packet := range map[string]string{"attribute": description(attrs, ""), "element": description("", body)} {
				findings := Findings([]byte(packet), "W")
				if got := value(t, findings, "GPS coordinates"); got != tt.want {
					t.Errorf("%s form: coordinates = %q, want %q", name, got, tt.want)
				}
				n := 0
				for _, f := range findings {
					if f.Field == "GPS coordinates" {
						n++
						if f.Category != core.CatLocation || f.Risk != core.RiskHigh || f.Where != "W/exif:GPS" {
							t.Errorf("finding = %+v", f)
						}
					}
				}
				if n != 1 {
					t.Errorf("%s form: %d coordinate findings, want one for the pair", name, n)
				}
			}
		})
	}
}

func TestParseCoordinate(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"48,51.5N", 48 + 51.5/60, true},
		{"48,51.5S", -(48 + 51.5/60), true},
		{"2,17.67E", 2 + 17.67/60, true},
		{"2,17.67W", -(2 + 17.67/60), true},
		{"48,51,30N", 48 + 51.0/60 + 30.0/3600, true},
		{"48,51,30S", -(48 + 51.0/60 + 30.0/3600), true},
		{"48,51.5n", 48 + 51.5/60, true},
		{"48,51.5s", -(48 + 51.5/60), true},
		{"  48,51.5N  ", 48 + 51.5/60, true},
		{"48, 51.5 N", 48 + 51.5/60, true},
		{"48N", 48, true},
		{"48S", -48, true},
		{"48.8584", 48.8584, true},
		{"-48.8584", -48.8584, true},
		{"0,0N", 0, true},
		{"0,0S", 0, true},
		{"180,0E", 180, true},
		{"180,0W", -180, true},
		// A writer that also used a sign: the sign covers the whole value, and
		// the two ways of saying south do not cancel out.
		{"-48,51.5", -(48 + 51.5/60), true},
		{"-48,51,30", -(48 + 51.0/60 + 30.0/3600), true},
		{"-48,51.5S", -(48 + 51.5/60), true},
		{"-0,30", -0.5, true},

		{"", 0, false},
		{"   ", 0, false},
		{"N", 0, false},
		{",N", 0, false},
		{"48,N", 0, false},
		{"abc", 0, false},
		{"48,abcN", 0, false},
		{"1,2,3,4N", 0, false},
		{"181,0E", 0, false},
		{"179,120E", 179 + 120.0/60, false},
		{"-181", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"Infinity", 0, false},
		{"-Inf", 0, false},
		{"1e400", 0, false},
		{"48,NaNN", 0, false},
		{"48,InfE", 0, false},
		// Only the degrees may carry a sign. Anything else is not a coordinate.
		{"48,-51.5N", 0, false},
		{"48,51,-30N", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseCoordinate(tt.in)
			if ok != tt.ok {
				t.Fatalf("parseCoordinate(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if tt.ok && got != tt.want {
				t.Errorf("parseCoordinate(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestZeroSouthIsNotNegativeZero(t *testing.T) {
	packet := description(`exif:GPSLatitude="0,0S" exif:GPSLongitude="0,0W"`, "")
	if got := value(t, Findings([]byte(packet), "W"), "GPS coordinates"); got != "0.000000, 0.000000" {
		t.Errorf("coordinates = %q", got)
	}
}

func TestNamespaces(t *testing.T) {
	t.Run("a renamed prefix is resolved", func(t *testing.T) {
		packet := `<r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:who="http://purl.org/dc/elements/1.1/"><r:Description who:creator="Test Author"><who:title><r:Alt><r:li>Test Title</r:li></r:Alt></who:title></r:Description></r:RDF>`
		findings := Findings([]byte(packet), "W")
		if got := value(t, findings, "Creator"); got != "Test Author" {
			t.Errorf("Creator = %q", got)
		}
		if got := value(t, findings, "Title"); got != "Test Title" {
			t.Errorf("Title = %q", got)
		}
	})
	t.Run("a default namespace is resolved", func(t *testing.T) {
		packet := `<creator xmlns="http://purl.org/dc/elements/1.1/">Test Author</creator>`
		if got := value(t, Findings([]byte(packet), "W"), "Creator"); got != "Test Author" {
			t.Errorf("Creator = %q", got)
		}
	})
	t.Run("a missing declaration still matches by prefix", func(t *testing.T) {
		packet := `<rdf:RDF><rdf:Description dc:creator="Test Author"/></rdf:RDF>`
		if got := value(t, Findings([]byte(packet), "W"), "Creator"); got != "Test Author" {
			t.Errorf("Creator = %q", got)
		}
	})
	t.Run("the well-known prefix on another namespace is not that property", func(t *testing.T) {
		packet := `<rdf:RDF xmlns:dc="http://example.com/other/"><rdf:Description dc:creator="Test Author"/></rdf:RDF>`
		findings := Findings([]byte(packet), "W")
		if slices.Contains(fieldNames(findings), "Creator") {
			t.Errorf("findings = %q", fieldNames(findings))
		}
		if got := value(t, findings, "Other XMP properties"); got != "1" {
			t.Errorf("Other XMP properties = %q", got)
		}
	})
}

// encodings returns text in every encoding the package claims to read.
func encodings(text string) map[string][]byte {
	utf16Units := utf16.Encode([]rune(text))
	wide := func(order binary.AppendByteOrder, prefix []byte) []byte {
		out := bytes.Clone(prefix)
		for _, unit := range utf16Units {
			out = order.AppendUint16(out, unit)
		}
		return out
	}
	wider := func(order binary.AppendByteOrder, prefix []byte) []byte {
		out := bytes.Clone(prefix)
		for _, r := range text {
			out = order.AppendUint32(out, uint32(r))
		}
		return out
	}
	le, be := binary.LittleEndian, binary.BigEndian
	return map[string][]byte{
		"UTF-8":               []byte(text),
		"UTF-8 with a mark":   append([]byte{0xEF, 0xBB, 0xBF}, text...),
		"UTF-16LE with mark":  wide(le, []byte{0xFF, 0xFE}),
		"UTF-16BE with mark":  wide(be, []byte{0xFE, 0xFF}),
		"UTF-16LE no mark":    wide(le, nil),
		"UTF-16BE no mark":    wide(be, nil),
		"UTF-32LE with mark":  wider(le, []byte{0xFF, 0xFE, 0, 0}),
		"UTF-32BE with mark":  wider(be, []byte{0, 0, 0xFE, 0xFF}),
		"UTF-32LE no mark":    wider(le, nil),
		"UTF-32BE no mark":    wider(be, nil),
		"after a JPEG header": append([]byte("http://ns.adobe.com/xap/1.0/\x00"), text...),
	}
}

func TestEncodings(t *testing.T) {
	text := string(coretest.FakeXMP())
	for name, packet := range encodings(text) {
		t.Run(name, func(t *testing.T) {
			findings := Findings(packet, "W")
			checkFindings(t, findings)
			if got := value(t, findings, "Creator"); got != "Test Author" {
				t.Errorf("Creator = %q", got)
			}
			if got := value(t, findings, "GPS coordinates"); got != coretest.EiffelTower {
				t.Errorf("coordinates = %q", got)
			}
			if got := value(t, findings, "Edit history"); got != "2 events" {
				t.Errorf("history = %q", got)
			}
			if !Contains(packet) {
				t.Error("Contains missed it")
			}
		})
	}
}

func TestNonASCIIValuesSurviveEveryEncoding(t *testing.T) {
	text := description(`dc:creator="Müller 東京 😀"`, "")
	for name, packet := range encodings(text) {
		t.Run(name, func(t *testing.T) {
			if got := value(t, Findings(packet, "W"), "Creator"); got != "Müller 東京 😀" {
				t.Errorf("Creator = %q", got)
			}
		})
	}
}

func TestDeclaredEncodingsDoNotStopParsing(t *testing.T) {
	for _, encoding := range []string{"UTF-8", "utf-8", "UTF-16", "ISO-8859-1", "x-made-up"} {
		t.Run(encoding, func(t *testing.T) {
			packet := `<?xml version="1.0" encoding="` + encoding + `"?>` + description(`dc:creator="Test Author"`, "")
			w := newWalker()
			if err := parseXML(decodeText([]byte(packet)), w); err != nil {
				t.Errorf("parseXML: %v", err)
			}
			if got := value(t, Findings([]byte(packet), "W"), "Creator"); got != "Test Author" {
				t.Errorf("Creator = %q", got)
			}
		})
	}
}

func TestPaddingAfterThePacketIsNotDamage(t *testing.T) {
	root := description(`dc:creator="Test Author"`, "")
	for name, tail := range map[string]string{
		"spaces":       strings.Repeat(" ", 4096),
		"NUL bytes":    strings.Repeat("\x00", 4096),
		"end marker":   `<?xpacket end="w"?>`,
		"stray text":   "garbage",
		"invalid byte": "\xFF\xFE",
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseXML(decodeText([]byte(root+tail)), newWalker()); err != nil {
				t.Errorf("parseXML: %v", err)
			}
			if got := value(t, Findings([]byte(root+tail), "W"), "Creator"); got != "Test Author" {
				t.Errorf("Creator = %q", got)
			}
		})
	}
}

func TestMalformedPacketsAreStillRead(t *testing.T) {
	tests := []struct {
		name   string
		packet string
		field  string
		want   string
	}{
		{"cut off inside an element value", `<rdf:RDF><rdf:Description><dc:creator><rdf:Seq><rdf:li>Test Aut`, "Creator", "Test Aut"},
		{"cut off inside an attribute value", `<rdf:RDF><rdf:Description dc:creator="Test Aut`, "Creator", "Test Aut"},
		{"cut off after an attribute", `<rdf:RDF><rdf:Description dc:creator="Test Author" `, "Creator", "Test Author"},
		{"mismatched end tag", `<rdf:RDF><rdf:Description dc:creator="Test Author"></wrong></rdf:RDF>`, "Creator", "Test Author"},
		{"end tag with nothing open", `</rdf:RDF><dc:creator>Test Author</dc:creator>`, "Creator", "Test Author"},
		{"value followed by damage", `<rdf:Description><dc:creator>Test Author</dc:creator></oops></rdf:Description>`, "Creator", "Test Author"},
		{"unquoted attribute", `<rdf:RDF><rdf:Description dc:creator=Test></rdf:Description>`, "Creator", "Test"},
		{"invalid UTF-8 in the value", "<rdf:RDF><rdf:Description dc:creator=\"Test\xFF Author\"></rdf:Description>", "Creator", "Test Author"},
		{"NUL in the middle", "<rdf:RDF>\x00<rdf:Description dc:creator=\"Test Author\"/></rdf:RDF>", "Creator", "Test Author"},
		{"coordinates in a damaged packet", `<rdf:Description exif:GPSLatitude="33,51.402S" exif:GPSLongitude="74,2.67W">`, "GPS coordinates", "-33.856700, -74.044500"},
		{"entities in a damaged packet", `<rdf:Description dc:creator="Tom &amp; Jerry"`, "Creator", "Tom & Jerry"},
		{"CDATA in a damaged packet", `<root><dc:creator><![CDATA[Tom <b> Jerry]]></dc:creator></oops>`, "Creator", "Tom <b> Jerry"},
		{"unterminated CDATA", `<dc:creator><![CDATA[Test Author`, "Creator", "Test Author"},
		{"comment in a damaged packet", `<root><!-- <dc:creator>Not This</dc:creator> --><dc:creator>Test Author</dc:creator></oops>`, "Creator", "Test Author"},
		{"list in a damaged packet", `<dc:subject><rdf:Bag><rdf:li>One</rdf:li><rdf:li>Two</rdf:li></rdf:Bag></oops>`, "Keywords", "One, Two"},
		{"history in a damaged packet", `<xmpMM:History><rdf:Seq><rdf:li a="1"/><rdf:li a="2"/></rdf:Seq></oops>`, "Edit history", "2 events"},
		{"a stray less-than sign", `<root><dc:creator>Test Author</dc:creator> < <<< </oops>`, "Creator", "Test Author"},
		{"stray quote in a tag", `<rdf:Description " dc:creator="Test Author"></oops>`, "Creator", "Test Author"},
		{"stray slash in a tag", `<rdf:Description / dc:creator="Test Author"></oops>`, "Creator", "Test Author"},
		{"spaces around the equals sign", `<rdf:Description dc:creator = "Test Author"></oops>`, "Creator", "Test Author"},
		{"attribute with no value before a real one", `<rdf:Description broken dc:creator="Test Author"></oops>`, "Creator", "Test Author"},
		{"single-quoted attribute", `<rdf:Description dc:creator='Test Author'></oops>`, "Creator", "Test Author"},
		{"self-closing tag", `<root><dc:creator/><dc:title>Test Title</dc:title></oops>`, "Title", "Test Title"},
		{"XML version the parser refuses", `<?xml version="1.1"?><dc:creator>Test Author</dc:creator>`, "Creator", "Test Author"},
		{"unquoted attribute at the end of the text", `<rdf:Description dc:creator=Test`, "Creator", "Test"},
		{"equals sign at the end of the text", `<rdf:Description dc:title="Test Title" dc:creator=`, "Title", "Test Title"},
		{"processing instruction and doctype", `<?php x?><!DOCTYPE y><root><dc:creator>Test Author</dc:creator></oops>`, "Creator", "Test Author"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Findings([]byte(tt.packet), "W")
			if got := value(t, findings, tt.field); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
			}
			checkPacket(t, []byte(tt.packet))

			// The non-strict parser repairs some of these by itself, so the scan is
			// driven directly to be sure it can read every one.
			w := newWalker()
			scan(decodeText([]byte(tt.packet)), w)
			if got := value(t, w.findings("W"), tt.field); got != tt.want {
				t.Errorf("scan: %s = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

// TestDamageDoesNotHideLaterProperties covers the packets most likely to fool a
// parser into reporting success: its damage comes first, and what the user most
// needs to hear about comes after it.
func TestDamageDoesNotHideLaterProperties(t *testing.T) {
	tests := []struct {
		name   string
		packet string
	}{
		{"a stray end tag in the middle", `<root><dc:creator>Test Author</dc:creator></oops><dc:title>Test Title</dc:title></root>`},
		{"a mismatched end tag after the property", `<root><dc:creator>Test Author</dc:creator></dc:wrong><dc:title>Test Title</dc:title></root>`},
		{"a stray end tag after the root closed", `<dc:creator>Test Author</dc:creator></oops><dc:title>Test Title</dc:title>`},
		{"a NUL after the root closed", "<dc:creator>Test Author</dc:creator>\x00<dc:title>Test Title</dc:title>"},
		{"a broken tag after the root closed", `<dc:creator>Test Author</dc:creator><"><dc:title>Test Title</dc:title>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Findings([]byte(tt.packet), "W")
			if got := value(t, findings, "Creator"); got != "Test Author" {
				t.Errorf("Creator = %q", got)
			}
			if got := value(t, findings, "Title"); got != "Test Title" {
				t.Errorf("Title = %q", got)
			}
		})
	}
}

// TestPropertyLeftOpenStillShowsWhatFollows pins down the scan's limit: with its
// end tag gone a property runs on into the next, so the second value is reported
// under the first property's name. It is misfiled, but never hidden.
func TestPropertyLeftOpenStillShowsWhatFollows(t *testing.T) {
	packet := `<root><dc:creator>Test Author</dc:wrong><dc:title>Test Title</dc:title></root>`
	got := value(t, Findings([]byte(packet), "W"), "Creator")
	if !strings.Contains(got, "Test Author") || !strings.Contains(got, "Test Title") {
		t.Errorf("Creator = %q", got)
	}
}

func TestEntitiesInWellFormedPackets(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"XML entities", "Tom &amp; Jerry &lt;x&gt;", "Tom & Jerry <x>"},
		{"HTML entities are common in the wild", "Caf&eacute;&nbsp;Test", "Caf\u00e9\u00a0Test"},
		{"an unknown entity is shown as written", "Test &bogus; Author", "Test &bogus; Author"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, packet := range map[string]string{
				"element":   description("", "<dc:creator>"+tt.text+"</dc:creator>"),
				"attribute": description(`dc:creator="`+tt.text+`"`, ""),
			} {
				if got := value(t, Findings([]byte(packet), "W"), "Creator"); got != tt.want {
					t.Errorf("%s form: Creator = %q, want %q", name, got, tt.want)
				}
			}
		})
	}
}

func TestAnyInputGivesAtLeastOneFinding(t *testing.T) {
	tests := []struct {
		name      string
		packet    []byte
		wantValue string
	}{
		{"nil", nil, "0 bytes, not well-formed"},
		{"empty", []byte{}, "0 bytes, not well-formed"},
		{"one NUL", []byte{0}, "1 bytes, not well-formed"},
		{"binary noise", []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10}, "6 bytes, not well-formed"},
		{"plain text", []byte("not xml"), "7 bytes, not well-formed"},
		{"only a less-than sign", []byte("<"), "1 bytes, not well-formed"},
		{"only padding", bytes.Repeat([]byte{' '}, 100), "100 bytes, not well-formed"},
		{"only a mark", []byte{0xEF, 0xBB, 0xBF}, "3 bytes, not well-formed"},
		{"odd-length UTF-16", []byte{0xFF, 0xFE, '<'}, "3 bytes, not well-formed"},
		{"well-formed with no properties", []byte("<a/>"), "4 bytes"},
		{"an empty description", []byte(description("", "")), ""},
		{"only an xpacket wrapper", []byte(`<?xpacket begin=""?><?xpacket end="w"?>`), "39 bytes, not well-formed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Findings(tt.packet, "APP1")
			if len(findings) != 1 {
				t.Fatalf("findings = %+v", findings)
			}
			f := findings[0]
			if f.Field != "XMP packet" || f.Action != core.ActionRemove || f.Where != "APP1" || f.Category != core.CatHidden {
				t.Errorf("finding = %+v", f)
			}
			if tt.wantValue != "" && f.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", f.Value, tt.wantValue)
			}
		})
	}
}

func TestEveryTruncationIsSafe(t *testing.T) {
	packet := coretest.FakeXMP()
	for n := range len(packet) + 1 {
		checkPacket(t, packet[:n])
	}
	for name, encoded := range encodings(string(packet[:400])) {
		for n := range len(encoded) + 1 {
			checkPacket(t, encoded[:n])
		}
		if t.Failed() {
			t.Fatalf("%s: a truncated packet broke an invariant", name)
		}
	}
}

func TestEveryByteCorruptionIsSafe(t *testing.T) {
	packet := coretest.FakeXMP()
	mutated := bytes.Clone(packet)
	for i := range packet {
		for _, b := range []byte{0x00, '<', '>', '"', '/', '&', 0xFF, packet[i] ^ 0xFF} {
			mutated[i] = b
			checkPacket(t, mutated)
		}
		mutated[i] = packet[i]
		if t.Failed() {
			t.Fatalf("corrupting byte %d broke an invariant", i)
		}
	}
}

func TestHostilePackets(t *testing.T) {
	t.Run("nesting a million levels deep", func(t *testing.T) {
		deep := []byte(strings.Repeat("<a>", 1000000))
		checkPacket(t, deep)
		checkPacket(t, []byte(strings.Repeat("<dc:creator>", 1000000)))
		checkPacket(t, []byte(strings.Repeat("<rdf:li>", 1000000)))
	})
	t.Run("many tiny attributes", func(t *testing.T) {
		checkPacket(t, []byte("<a "+strings.Repeat("b ", 1<<20)+">"))
		checkPacket(t, []byte(`<a `+strings.Repeat(`b="" `, 1<<18)+`/>`))
	})
	t.Run("many values for one property are capped", func(t *testing.T) {
		var items strings.Builder
		for i := range 10000 {
			items.WriteString("<rdf:li>value ")
			items.WriteString(strings.Repeat("x", i%7))
			items.WriteString(string(rune('a' + i%26)))
			items.WriteString("</rdf:li>")
		}
		w := newWalker()
		packet := description("", "<dc:subject><rdf:Bag>"+items.String()+"</rdf:Bag></dc:subject>")
		if err := parseXML(packet, w); err != nil {
			t.Fatal(err)
		}
		if got := len(w.values[propertyIndex["dc:subject"]]); got != maxValuesPerProperty {
			t.Errorf("kept %d values, want %d", got, maxValuesPerProperty)
		}
		checkPacket(t, []byte(packet))
	})
	t.Run("a huge value in each form stays bounded", func(t *testing.T) {
		huge := strings.Repeat("A", 2<<20)
		for name, packet := range map[string]string{
			"element":          description("", "<dc:creator>"+huge+"</dc:creator>"),
			"attribute":        description(`dc:creator="`+huge+`"`, ""),
			"damaged element":  "<dc:creator>" + huge + "</oops>",
			"damaged attr":     `<a dc:creator="` + huge,
			"unknown property": description(`custom:x="`+huge+`"`, ""),
		} {
			t.Run(name, func(t *testing.T) {
				checkPacket(t, []byte(packet))
				w := newWalker()
				if parseXML(decodeText([]byte(packet)), w) != nil {
					w = newWalker()
					scan(decodeText([]byte(packet)), w)
				}
				for i, values := range w.values {
					for _, v := range values {
						if len(v) > maxValueBytes {
							t.Errorf("%s holds a value of %d bytes", properties[i].name, len(v))
						}
					}
				}
			})
		}
	})
	t.Run("a packet over the size cap still reports itself", func(t *testing.T) {
		padded := append(bytes.Repeat([]byte{' '}, maxPacketBytes), description(`dc:creator="Hidden"`, "")...)
		findings := Findings(padded, "W")
		if len(findings) != 1 || findings[0].Field != "XMP packet" {
			t.Errorf("findings = %+v", findings)
		}
		if !strings.HasPrefix(findings[0].Value, strconv.Itoa(len(padded))+" bytes") {
			t.Errorf("value = %q does not give the real size", findings[0].Value)
		}
	})
	t.Run("entities are never expanded", func(t *testing.T) {
		laughs := `<!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">` +
			`<!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">]>`
		packet := []byte(laughs + `<dc:creator>&lol3;</dc:creator>`)
		findings := Findings(packet, "W")
		if got := value(t, findings, "Creator"); got != "&lol3;" {
			t.Errorf("Creator = %q", got)
		}
	})
	t.Run("an external entity is not fetched", func(t *testing.T) {
		packet := []byte(`<!DOCTYPE x [<!ENTITY e SYSTEM "file:///etc/passwd">]><dc:creator>&e;</dc:creator>`)
		if got := value(t, Findings(packet, "W"), "Creator"); got != "&e;" {
			t.Errorf("Creator = %q", got)
		}
	})
}

func TestValuesAreSanitised(t *testing.T) {
	tests := []struct {
		name   string
		packet string
		want   string
	}{
		{"raw escape in a damaged packet", "<dc:creator>\x1b[31mEvil</oops>", "[31mEvil"},
		{"character reference to an escape", `<rdf:Description dc:creator="&#27;[31mEvil"></oops>`, "[31mEvil"},
		{"hex reference to a bidi override", description(`dc:creator="good&#x202E;txt.exe"`, ""), "goodtxt.exe"},
		{"element with a bidi override", description("", "<dc:creator>good\u202Etxt.exe</dc:creator>"), "goodtxt.exe"},
		{"newlines cannot forge a report line", description("", "<dc:creator>x\nGPS: none</dc:creator>"), "x GPS: none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := Findings([]byte(tt.packet), "W")
			checkFindings(t, findings)
			if got := value(t, findings, "Creator"); got != tt.want {
				t.Errorf("Creator = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("a hostile where is sanitised", func(t *testing.T) {
		for _, f := range Findings([]byte("x"), "evil\x1b[2J") {
			if f.Where != "evil[2J" {
				t.Errorf("Where = %q", f.Where)
			}
		}
	})
}

// TestBothPathsAgree holds the parser and the tolerant scan to the same answer on
// packets each can read, since a damaged packet should report what a whole one
// would have.
func TestBothPathsAgree(t *testing.T) {
	packets := map[string]string{
		"fake packet": string(coretest.FakeXMP()),
		"lists":       description("", `<dc:subject><rdf:Bag><rdf:li>One</rdf:li><rdf:li>Two</rdf:li></rdf:Bag></dc:subject><dc:creator><rdf:Seq><rdf:li>A</rdf:li></rdf:Seq></dc:creator>`),
		"attributes":  description(`dc:creator="A &amp; B" exif:GPSLatitude="48,51.5N" custom:other="1"`, ""),
		"history":     description("", `<xmpMM:History><rdf:Seq><rdf:li stEvt:action="saved"/><rdf:li><rdf:Description><stEvt:changed><rdf:Bag><rdf:li>x</rdf:li></rdf:Bag></stEvt:changed></rdf:Description></rdf:li></rdf:Seq></xmpMM:History>`),
		"resource":    description("", `<dc:creator rdf:resource="http://example.com/"/>`),
	}
	for name, packet := range packets {
		t.Run(name, func(t *testing.T) {
			parsed, scanned := newWalker(), newWalker()
			if err := parseXML(packet, parsed); err != nil {
				t.Fatal(err)
			}
			scan(packet, scanned)
			a, b := parsed.findings("W"), scanned.findings("W")
			if !slices.Equal(a, b) {
				t.Errorf("parser and scan disagree:\nparser: %+v\nscan:   %+v", a, b)
			}
		})
	}
}

func TestContains(t *testing.T) {
	positive := []string{
		"<x:xmpmeta xmlns:x=\"adobe:ns:meta/\">",
		"<x:xapmeta",
		`<?xpacket begin=""?>`,
		"prefix http://ns.adobe.com/xap/1.0/ suffix",
		"<rdf:RDF>",
		// The prefixes are free to change, so the namespaces count as well.
		`<meta:xmpmeta xmlns:meta="adobe:ns:meta/">`,
		`<r:RDF xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#">`,
	}
	for _, text := range positive {
		for name, encoded := range encodings(text) {
			t.Run(text+"/"+name, func(t *testing.T) {
				if !Contains(encoded) {
					t.Errorf("Contains(%q) = false", encoded)
				}
			})
		}
		wrapped := append(append([]byte("binary\x00\xFF"), text...), "\x00more"...)
		if !Contains(wrapped) {
			t.Errorf("Contains missed %q inside other bytes", text)
		}
	}
	negative := [][]byte{
		nil,
		{},
		[]byte("plain text about xmpmeta and rdf"),
		[]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"),
		{0xFF, 0xD8, 0xFF, 0xE0},
		[]byte("<x:xmpmet"),
	}
	for _, data := range negative {
		if Contains(data) {
			t.Errorf("Contains(%q) = true", data)
		}
	}
}

func TestDecodeText(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"plain", []byte("<a/>"), "<a/>"},
		{"a mark", []byte("\xEF\xBB\xBF<a/>"), "<a/>"},
		{"a leading signature", []byte("sig\x00<a/>"), "<a/>"},
		{"text with no tag stays", []byte("no tag"), "no tag"},
		{"odd-length UTF-16 drops the last byte", []byte{0xFF, 0xFE, '<', 0, 'a'}, "<"},
		{"short UTF-32 drops what does not fit", []byte{0xFF, 0xFE, 0, 0, '<', 0, 0}, ""},
		{"an unpaired surrogate is replaced", []byte{0xFF, 0xFE, 0x00, 0xD8, '<', 0}, "<"},
		{"an out-of-range UTF-32 value is replaced", []byte{0, 0, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, '<'}, "<"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeText(tt.in); got != tt.want {
				t.Errorf("decodeText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
