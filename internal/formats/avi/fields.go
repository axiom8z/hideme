package avi

import "github.com/axiom8z/hideme/internal/core"

// finding describes how a chunk appears in the report.
type finding struct {
	field    string
	category core.Category
	risk     core.Risk
}

var (
	findingTrailing  = finding{"Trailing data", core.CatHidden, core.RiskHigh}
	findingJunk      = finding{"Hidden data in JUNK chunk", core.CatHidden, core.RiskMed}
	findingSlack     = finding{"Hidden data in a header chunk", core.CatHidden, core.RiskMed}
	findingBadIndex  = finding{"Index that does not match the file", core.CatHidden, core.RiskMed}
	findingUnknown   = finding{"Unknown chunk", core.CatHidden, core.RiskMed}
	findingInfo      = finding{"INFO list", core.CatContent, core.RiskLow}
	findingOtherInfo = finding{"Other INFO entry", core.CatContent, core.RiskLow}
)

// namedChunks are the header chunks known to carry metadata. They are not an
// allowlist: every chunk the allowlists do not name is removed, and this table
// only gives the known ones a better description.
var namedChunks = map[string]struct {
	finding finding
	isText  bool
}{
	"IDIT": {finding{"Capture date", core.CatTime, core.RiskMed}, true},
	"ISMP": {finding{"Timecode", core.CatTime, core.RiskMed}, true},
	"strn": {finding{"Stream name", core.CatContent, core.RiskLow}, true},
	"strd": {finding{"Stream vendor data", core.CatDevice, core.RiskMed}, false},
	"vedt": {finding{"Video editing data", core.CatSoftware, core.RiskLow}, false},
}

// infoFields names the entries of a LIST INFO, as defined by the RIFF
// specification and its later Exif and DV extensions.
var infoFields = map[string]finding{
	"IART": {"Artist", core.CatIdentity, core.RiskHigh},
	"IENG": {"Engineer", core.CatIdentity, core.RiskHigh},
	"ITCH": {"Technician", core.CatIdentity, core.RiskHigh},
	"ICMS": {"Commissioned by", core.CatIdentity, core.RiskHigh},
	"ICOP": {"Copyright", core.CatIdentity, core.RiskHigh},
	"IWRI": {"Written by", core.CatIdentity, core.RiskHigh},
	"IPRO": {"Produced by", core.CatIdentity, core.RiskHigh},
	"IDIR": {"Directed by", core.CatIdentity, core.RiskHigh},
	"ILOC": {"Location", core.CatLocation, core.RiskHigh},
	"IARL": {"Archival location", core.CatLocation, core.RiskMed},
	"ICRD": {"Creation date", core.CatTime, core.RiskMed},
	"IDIT": {"Capture date", core.CatTime, core.RiskMed},
	"ISMP": {"Timecode", core.CatTime, core.RiskMed},
	"ISFT": {"Software", core.CatSoftware, core.RiskLow},
	"ITOC": {"Table of contents", core.CatContent, core.RiskLow},
	"ICMT": {"Comment", core.CatContent, core.RiskMed},
	"ISBJ": {"Subject", core.CatContent, core.RiskMed},
	"INAM": {"Title", core.CatContent, core.RiskLow},
	"IKEY": {"Keywords", core.CatContent, core.RiskLow},
	"IGNR": {"Genre", core.CatContent, core.RiskLow},
	"IPRD": {"Product", core.CatContent, core.RiskLow},
	"ISRC": {"Source", core.CatContent, core.RiskLow},
	"ISRF": {"Source form", core.CatContent, core.RiskLow},
	"IMED": {"Medium", core.CatContent, core.RiskLow},
	"ILNG": {"Language", core.CatContent, core.RiskLow},
	"ITRK": {"Track number", core.CatContent, core.RiskLow},
}
