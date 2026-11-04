package coretest

import (
	"encoding/binary"
	"unicode/utf16"
)

// EiffelTower is the fake location in FakeEXIF and FakeXMP, as the decimal
// degrees the exif and xmp packages report.
const EiffelTower = "48.858400, 2.294500"

// fakeThumbnail stands in for a JPEG preview. It is distinctive so that a test
// can see whether the bytes survived.
var fakeThumbnail = []byte("\xFF\xD8Test Thumbnail Secret\xFF\xD9")

// fakeMakerNote stands in for vendor data, which commonly holds a serial number.
var fakeMakerNote = []byte("Test MakerNote Serial 0123456789")

// FakeIFD returns a directory tree with obviously fake personal data in every
// position the exif package reports. An orientation of 0 omits that tag. Tests
// that need one byte order, or want to change a value, start from this; most
// use FakeEXIF.
func FakeIFD(orientation uint16) *IFD {
	root := &IFD{
		Entries: []Entry{
			{Tag: 0x010D, Value: "Test Document"},
			{Tag: 0x010E, Value: "Test Description"},
			{Tag: 0x010F, Value: "Test Make"},
			{Tag: 0x0110, Value: "Test Model"},
			{Tag: 0x011A, Value: [][2]uint32{{72, 1}}}, // XResolution, a catch-all example
			{Tag: 0x0131, Value: "Test Software"},
			{Tag: 0x0132, Value: "2001:02:03 04:05:06"},
			{Tag: 0x013B, Value: "Test Author"},
			{Tag: 0x013C, Value: "Test Host"},
			{Tag: 0x8298, Value: "Test Copyright"},
			{Tag: 0x9C9B, Type: TypeByte, Value: utf16LE("Test XP Title")},
			{Tag: 0x9C9C, Type: TypeByte, Value: utf16LE("Test XP Comment")},
			{Tag: 0x9C9D, Type: TypeByte, Value: utf16LE("Test XP Author")},
			{Tag: 0x9C9E, Type: TypeByte, Value: utf16LE("Test XP Keywords")},
			{Tag: 0x9C9F, Type: TypeByte, Value: utf16LE("Test XP Subject")},
		},
		Exif: &IFD{
			Entries: []Entry{
				{Tag: 0x9003, Value: "2001:02:03 04:05:07"},
				{Tag: 0x9004, Value: "2001:02:03 04:05:08"},
				{Tag: 0x9010, Value: "+01:00"},
				{Tag: 0x9011, Value: "+02:00"},
				{Tag: 0x9012, Value: "+03:00"},
				{Tag: 0x927C, Value: fakeMakerNote},
				{Tag: 0x9286, Value: append([]byte("ASCII\x00\x00\x00"), "Test Comment"...)},
				{Tag: 0xA420, Value: "0123456789abcdef0123456789abcdef"},
				{Tag: 0xA430, Value: "Test Owner"},
				{Tag: 0xA431, Value: "TESTBODY123"},
				{Tag: 0xA433, Value: "Test Lens Make"},
				{Tag: 0xA434, Value: "Test Lens Model"},
				{Tag: 0xA435, Value: "TESTLENS456"},
			},
			Interop: &IFD{Entries: []Entry{{Tag: 0x0001, Value: "R98"}}},
		},
		GPS: &IFD{Entries: []Entry{
			{Tag: 0x0000, Type: TypeByte, Value: []byte{2, 3, 0, 0}},
			{Tag: 0x0001, Value: "N"},
			{Tag: 0x0002, Value: [][2]uint32{{48, 1}, {51, 1}, {3024, 100}}},
			{Tag: 0x0003, Value: "E"},
			{Tag: 0x0004, Value: [][2]uint32{{2, 1}, {17, 1}, {402, 10}}},
			{Tag: 0x0005, Type: TypeByte, Value: []byte{0}},
			{Tag: 0x0006, Value: [][2]uint32{{330, 1}}},
			{Tag: 0x0007, Value: [][2]uint32{{4, 1}, {5, 1}, {6, 1}}},
			{Tag: 0x001D, Value: "2001:02:03"},
		}},
		Next: &IFD{
			Entries:   []Entry{{Tag: 0x0103, Value: uint16(6)}},
			Thumbnail: fakeThumbnail,
		},
	}
	if orientation != 0 {
		root.Entries = append(root.Entries, Entry{Tag: 0x0112, Value: orientation})
	}
	return root
}

// FakeEXIF returns a raw TIFF block, without the JPEG "Exif\0\0" prefix, built
// from FakeIFD. Handlers embed it in their fixtures and check the result with
// FakeEXIFFields and FakeEXIFSecrets.
func FakeEXIF(orientation uint16) []byte {
	return FakeIFD(orientation).Encode(binary.LittleEndian)
}

// FakeEXIFFields returns the Finding.Field values Inspect must report for a
// block from FakeEXIF, not including Orientation.
func FakeEXIFFields() []string {
	return []string{
		"GPS coordinates", "GPS altitude", "GPS timestamp", "GPS date",
		"Camera make", "Camera model", "Lens make", "Lens model",
		"Body serial number", "Lens serial number", "Owner name", "Artist",
		"Copyright", "Software", "Host computer", "Document name",
		"Image description", "User comment",
		"XP title", "XP comment", "XP author", "XP keywords", "XP subject",
		"Date modified", "Date taken", "Date digitized",
		"Time zone", "Time zone taken", "Time zone digitized",
		"Image unique ID", "MakerNote", "Embedded thumbnail",
		"Other GPS tags", "Other EXIF tags",
	}
}

// FakeEXIFSecrets returns byte strings from FakeEXIF that must not survive
// cleaning, ready for Case.MustNotContain.
func FakeEXIFSecrets() [][]byte {
	return asBytes(
		"Test Author", "Test Make", "Test Model", "Test Software", "Test Host",
		"Test Copyright", "Test Document", "Test Description", "Test Comment",
		"Test Owner", "TESTBODY123", "TESTLENS456", "Test Lens Make",
		"Test Lens Model", "2001:02:03", "0123456789abcdef",
		string(fakeThumbnail), string(fakeMakerNote),
		string(utf16LE("Test XP Author")),
	)
}

// FakeXMP returns a complete XMP packet with fake personal data, using both the
// attribute form and the element form that real writers produce.
func FakeXMP() []byte {
	return []byte(`<?xpacket begin="` + "\xEF\xBB\xBF" + `" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Test XMP Toolkit">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:dc="http://purl.org/dc/elements/1.1/"
    xmlns:xmp="http://ns.adobe.com/xap/1.0/"
    xmlns:xmpMM="http://ns.adobe.com/xap/1.0/mm/"
    xmlns:stEvt="http://ns.adobe.com/xap/1.0/sType/ResourceEvent#"
    xmlns:xmpRights="http://ns.adobe.com/xap/1.0/rights/"
    xmlns:exif="http://ns.adobe.com/exif/1.0/"
    xmlns:tiff="http://ns.adobe.com/tiff/1.0/"
    xmlns:aux="http://ns.adobe.com/exif/1.0/aux/"
    xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/"
    xmlns:Iptc4xmpCore="http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/"
    xmlns:pdf="http://ns.adobe.com/pdf/1.3/"
    xmp:CreatorTool="Test Editor 1.0"
    xmp:CreateDate="2001-02-03T04:05:06+01:00"
    xmp:ModifyDate="2001-02-03T04:05:07+01:00"
    xmp:MetadataDate="2001-02-03T04:05:08+01:00"
    exif:GPSLatitude="48,51.504N"
    exif:GPSLongitude="2,17.67E"
    tiff:Make="Test Make"
    tiff:Model="Test Model"
    aux:SerialNumber="TESTSERIAL123"
    aux:Lens="Test Lens"
    photoshop:City="Test City"
    photoshop:State="Test State"
    photoshop:Country="Test Country"
    photoshop:AuthorsPosition="Test Position"
    photoshop:Credit="Test Credit"
    Iptc4xmpCore:Location="Test Location"
    xmpMM:DocumentID="xmp.did:test-document"
    xmpMM:InstanceID="xmp.iid:test-instance"
    pdf:Producer="Test Producer"
    pdf:Author="Test PDF Author"
    xmpRights:Marked="True">
   <dc:creator><rdf:Seq><rdf:li>Test Author</rdf:li></rdf:Seq></dc:creator>
   <dc:rights><rdf:Alt><rdf:li xml:lang="x-default">Test Rights</rdf:li></rdf:Alt></dc:rights>
   <dc:title><rdf:Alt><rdf:li xml:lang="x-default">Test Title</rdf:li></rdf:Alt></dc:title>
   <dc:description><rdf:Alt><rdf:li xml:lang="x-default">Test Description</rdf:li></rdf:Alt></dc:description>
   <dc:subject><rdf:Bag><rdf:li>Test Keyword</rdf:li><rdf:li>Second Keyword</rdf:li></rdf:Bag></dc:subject>
   <xmpMM:History><rdf:Seq>
     <rdf:li stEvt:action="created" stEvt:softwareAgent="Test Editor 1.0"/>
     <rdf:li stEvt:action="saved" stEvt:softwareAgent="Test Editor 1.0"/>
   </rdf:Seq></xmpMM:History>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>`)
}

// FakeXMPFields returns the Finding.Field values Inspect must report for
// FakeXMP.
func FakeXMPFields() []string {
	return []string{
		"Creator", "Rights", "Title", "Description", "Keywords", "Creator tool",
		"Create date", "Modify date", "Metadata date", "GPS coordinates",
		"Camera make", "Camera model", "Serial number", "Lens",
		"City", "State", "Country", "Author's position", "Credit", "Location",
		"Edit history", "Document ID", "Instance ID",
		"PDF producer", "PDF author", "Rights marked",
	}
}

// FakeXMPSecrets returns byte strings from FakeXMP that must not survive
// cleaning, ready for Case.MustNotContain.
func FakeXMPSecrets() [][]byte {
	return asBytes(
		"Test Author", "Test Rights", "Test Title", "Test Keyword", "Test Editor",
		"48,51.504N", "TESTSERIAL123", "Test City", "xmp.did:test-document",
		"Test PDF Author", "x:xmpmeta", "xpacket",
	)
}

func utf16LE(s string) []byte {
	var out []byte
	for _, unit := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}
	return append(out, 0, 0)
}

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}
