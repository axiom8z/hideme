package xmp

import "github.com/axiom8z/hideme/internal/core"

// propertyKind says how a property's value turns into a finding.
type propertyKind int

const (
	kindText propertyKind = iota
	kindCount
	kindLatitude
	kindLongitude
)

// property describes one XMP property worth naming in a report.
type property struct {
	name     string // qualified with its canonical prefix
	field    string
	category core.Category
	risk     core.Risk
	kind     propertyKind
}

// properties lists what is reported individually, in report order. Everything
// else is counted in the "Other XMP properties" finding. Field names match the
// exif package where the meaning is the same, so a report reads consistently.
var properties = []property{
	{"exif:GPSLatitude", "GPS coordinates", core.CatLocation, core.RiskHigh, kindLatitude},
	{"exif:GPSLongitude", "GPS coordinates", core.CatLocation, core.RiskHigh, kindLongitude},
	{"exif:GPSAltitude", "GPS altitude", core.CatLocation, core.RiskMed, kindText},
	{"exif:GPSTimeStamp", "GPS timestamp", core.CatTime, core.RiskMed, kindText},
	{"photoshop:City", "City", core.CatLocation, core.RiskHigh, kindText},
	{"Iptc4xmpCore:Location", "Location", core.CatLocation, core.RiskHigh, kindText},
	{"photoshop:State", "State", core.CatLocation, core.RiskMed, kindText},
	{"photoshop:Country", "Country", core.CatLocation, core.RiskMed, kindText},

	{"dc:creator", "Creator", core.CatIdentity, core.RiskHigh, kindText},
	{"dc:contributor", "Contributor", core.CatIdentity, core.RiskHigh, kindText},
	{"dc:publisher", "Publisher", core.CatIdentity, core.RiskHigh, kindText},
	{"dc:rights", "Rights", core.CatIdentity, core.RiskHigh, kindText},
	{"pdf:Author", "PDF author", core.CatIdentity, core.RiskHigh, kindText},
	{"photoshop:Credit", "Credit", core.CatIdentity, core.RiskHigh, kindText},
	{"photoshop:CaptionWriter", "Caption writer", core.CatIdentity, core.RiskHigh, kindText},
	{"xmpRights:Owner", "Rights owner", core.CatIdentity, core.RiskHigh, kindText},
	{"aux:OwnerName", "Owner name", core.CatIdentity, core.RiskHigh, kindText},
	{"exifEX:CameraOwnerName", "Owner name", core.CatIdentity, core.RiskHigh, kindText},
	{"photoshop:AuthorsPosition", "Author's position", core.CatIdentity, core.RiskMed, kindText},
	{"xmpRights:WebStatement", "Rights URL", core.CatIdentity, core.RiskMed, kindText},

	// Creator contact details, which IPTC tools nest in a structure.
	{"Iptc4xmpCore:CiEmailWork", "Contact email", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiTelWork", "Contact phone", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiUrlWork", "Contact URL", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiAdrExtadr", "Contact address", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiAdrCity", "Contact city", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiAdrRegion", "Contact region", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiAdrPcode", "Contact postal code", core.CatIdentity, core.RiskHigh, kindText},
	{"Iptc4xmpCore:CiAdrCtry", "Contact country", core.CatIdentity, core.RiskHigh, kindText},

	{"aux:SerialNumber", "Serial number", core.CatDevice, core.RiskHigh, kindText},
	{"exifEX:BodySerialNumber", "Serial number", core.CatDevice, core.RiskHigh, kindText},
	{"exifEX:LensSerialNumber", "Lens serial number", core.CatDevice, core.RiskHigh, kindText},
	{"tiff:Make", "Camera make", core.CatDevice, core.RiskMed, kindText},
	{"tiff:Model", "Camera model", core.CatDevice, core.RiskMed, kindText},
	{"aux:Lens", "Lens", core.CatDevice, core.RiskMed, kindText},
	{"exifEX:LensModel", "Lens", core.CatDevice, core.RiskMed, kindText},

	{"exif:DateTimeOriginal", "Date taken", core.CatTime, core.RiskMed, kindText},
	{"photoshop:DateCreated", "Date taken", core.CatTime, core.RiskMed, kindText},
	{"xmp:CreateDate", "Create date", core.CatTime, core.RiskMed, kindText},
	{"xmp:ModifyDate", "Modify date", core.CatTime, core.RiskMed, kindText},
	{"xmp:MetadataDate", "Metadata date", core.CatTime, core.RiskMed, kindText},

	{"xmp:CreatorTool", "Creator tool", core.CatSoftware, core.RiskLow, kindText},
	{"pdf:Producer", "PDF producer", core.CatSoftware, core.RiskLow, kindText},
	{"x:xmptk", "XMP toolkit", core.CatSoftware, core.RiskLow, kindText},

	// The edit history and document identifiers tie this copy to its earlier
	// versions and to every other copy, even after the content has changed.
	{"xmpMM:History", "Edit history", core.CatHidden, core.RiskMed, kindCount},
	{"photoshop:History", "Edit history", core.CatHidden, core.RiskMed, kindText},
	{"xmpMM:DerivedFrom", "Derived from", core.CatHidden, core.RiskMed, kindText},
	{"xmpMM:DocumentID", "Document ID", core.CatHidden, core.RiskMed, kindText},
	{"xmpMM:OriginalDocumentID", "Original document ID", core.CatHidden, core.RiskMed, kindText},
	{"xmpMM:InstanceID", "Instance ID", core.CatHidden, core.RiskMed, kindText},

	{"dc:description", "Description", core.CatContent, core.RiskMed, kindText},
	{"exif:UserComment", "User comment", core.CatContent, core.RiskMed, kindText},
	{"dc:title", "Title", core.CatContent, core.RiskLow, kindText},
	{"dc:subject", "Keywords", core.CatContent, core.RiskLow, kindText},
	{"pdf:Keywords", "Keywords", core.CatContent, core.RiskLow, kindText},
	{"xmpRights:Marked", "Rights marked", core.CatContent, core.RiskLow, kindText},
}

// otherProperties describes the catch-all finding for properties not in the
// table. Its rating reflects that an unknown property could hold anything.
var otherProperties = property{field: "Other XMP properties", category: core.CatHidden, risk: core.RiskMed}

// propertyIndex finds a table row by qualified name.
var propertyIndex = func() map[string]int {
	index := make(map[string]int, len(properties))
	for i, p := range properties {
		index[p.name] = i
	}
	return index
}()
