package office

import (
	"archive/zip"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// A part's name is text from the file, and `clean` prints every warning and every
// finding's location, so a name such as word/media/Jane Doe passport.jpg must not
// reach them. The names Office and LibreOffice generate say nothing about anyone and
// are what makes a report readable, so they are shown. The test is an allowlist, as
// nothing else could keep out a name no one has thought of. Any other part is called
// by its position in the ZIP directory, and only its finding's value holds the name.
var (
	conventionalDirs = wordSet(
		"word", "xl", "ppt", "docprops", "customxml", "_rels", "media", "embeddings", "worksheets", "chartsheets",
		"dialogsheets", "macrosheets", "charts", "chartex", "drawings", "vmldrawings", "theme", "slides", "slidelayouts",
		"slidemasters", "notesslides", "notesmasters", "handoutmasters", "comments", "threadedcomments", "persons",
		"externallinks", "pivotcache", "pivottables", "tables", "printersettings", "revisions", "tags", "diagrams",
		"activex", "ctrlprops", "querytables", "glossary", "fonts", "ink", "model", "webextensions", "taskpanes",
		"customui", "images", "docmetadata", "changesinfos", "_xmlsignatures", "richdata", "slicers", "slicercaches",
		"timelines", "timelinecaches", "package", "services", "metadata", "core-properties",
		"meta-inf", "pictures", "thumbnails", "objectreplacements", "configurations2", "accelerator", "floater",
		"menubar", "popupmenu", "progressbar", "statusbar", "toolbar", "toolpanel", "bitmaps", "basic", "dialogs",
		"scripts", "standard", "python", "versions",
	)

	// conventionalStems are file names without their extension or any trailing number.
	conventionalStems = wordSet(
		"[content_types]", "document", "styles", "styleswitheffects", "settings", "websettings", "fonttable", "numbering",
		"footnotes", "endnotes", "header", "footer", "comments", "commentsextended", "commentsids", "commentsextensible",
		"people", "theme", "thememanager", "image", "media", "audio", "video", "hdphoto", "oleobject", "ole", "package",
		"workbook", "sheet", "sharedstrings", "calcchain", "connections", "drawing", "vmldrawing", "commentsdrawing", "chart", "chartsheet",
		"style", "colors", "comment", "threadedcomment", "person", "externallink", "pivotcachedefinition",
		"pivotcacherecords", "pivottable", "table", "querytable", "printersettings", "ctrlprop", "activex", "slicer",
		"slicercache", "timeline", "timelinecache", "metadata", "rdrichvalue", "rdrichvaluestructure", "rdrichvaluetypes",
		"rdarray", "richstyles", "xmlmaps", "volatiledependencies", "usernames", "revisionheaders", "revisionlog",
		"customproperty", "item", "itemprops", "vbaproject", "vbadata", "vbaprojectsignature", "customui", "customui14",
		"data", "layout", "quickstyle", "presentation", "presprops", "viewprops", "tablestyles", "slide", "slidelayout",
		"slidemaster", "notesslide", "notesmaster", "handoutmaster", "commentauthors", "authors", "revisioninfo",
		"changesinfo", "tag", "legacydoctextinfo", "font", "core", "app", "custom", "thumbnail", "labelinfo", "origin",
		"sig", "webextension", "taskpanes", "ink", "microsoft_excel_worksheet", "microsoft_excel_macro-enabled_worksheet",
		"microsoft_excel_binary_worksheet", "microsoft_excel_97-2003_worksheet", "microsoft_word_document",
		"microsoft_word_97_-_2003_document", "microsoft_powerpoint_presentation", "microsoft_powerpoint_97-2003_presentation",
		"microsoft_visio_drawing",
		"content", "meta", "manifest", "mimetype", "documentsignatures", "macrosignatures", "packagesignatures",
		"versionlist", "version", "current", "script-lc", "script-lb", "dialog-lc", "dialog-lb", "module", "object",
	)

	conventionalExts = wordSet(
		"", ".xml", ".rels", ".bin", ".vml", ".png", ".jpg", ".jpeg", ".jpe", ".gif", ".tif", ".tiff", ".bmp", ".emf",
		".wmf", ".svg", ".wdp", ".jxr", ".ico", ".mp3", ".m4a", ".wav", ".wma", ".mp4", ".m4v", ".avi", ".wmv", ".mov",
		".mpg", ".mpeg", ".xlsx", ".xlsm", ".xlsb", ".xls", ".docx", ".docm", ".doc", ".pptx", ".pptm", ".ppt", ".vsdx",
		".vsd", ".pdf", ".odt", ".ods", ".odp", ".odg", ".odttf", ".fntdata", ".sigs", ".rdf", ".data", ".psmdcp",
	)

	// Generated names: hashes, GUIDs, an ODF object's folder, and PowerPoint's modern
	// comments, which are named after numbers.
	generatedName = regexp.MustCompile(`^(?:[0-9a-f-]{16,}|object [0-9]+|obj[0-9]+|moderncomment_[0-9a-f_]+)$`)
)

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

// conventionalName reports whether every piece of a part's name is one that software
// generates, which also rules out percent-encoding and anything else unusual.
func conventionalName(name string) bool {
	key := strings.ToLower(strings.TrimSuffix(name, "/"))
	dir, base := path.Split(key)
	if dir != "" {
		for segment := range strings.SplitSeq(strings.TrimSuffix(dir, "/"), "/") {
			if !conventionalDirs[segment] && !generatedName.MatchString(segment) {
				return false
			}
		}
	}
	if strings.HasSuffix(name, "/") {
		return conventionalDirs[base] || generatedName.MatchString(base)
	}
	if inner, isRels := strings.CutSuffix(base, ".rels"); isRels {
		// document.xml.rels is about document.xml, and .rels about the package.
		if inner == "" {
			return true
		}
		base = inner
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if !conventionalExts[ext] {
		return false
	}
	return conventionalStems[strings.TrimRight(stem, "0123456789 ")] || generatedName.MatchString(stem)
}

// label is how reports refer to a part.
func (a *archive) label(f *zip.File) string {
	if conventionalName(f.Name) {
		return f.Name
	}
	return fmt.Sprintf("ZIP entry %d", a.index[f]+1)
}

// reportUnusualNames says which names stay in the output and may mean something.
// `inspect` shows them, since a finding's value is the one place that may hold them.
func (a *archive) reportUnusualNames(rep *core.Report, kept func(*zip.File) bool) {
	found := 0
	for _, f := range a.entries {
		if conventionalName(f.Name) || !kept(f) {
			continue
		}
		if found < maxValuesPerField {
			rep.Add(core.Finding{
				Category: core.CatHidden,
				Risk:     core.RiskLow,
				Field:    "Unusual part name",
				Value:    f.Name,
				Where:    a.label(f),
				Action:   core.ActionRetain,
				Reason:   "documents find their parts by name; it can hold personal text, so review it before sharing",
			})
		}
		found++
	}
	if found > 0 {
		rep.Warn("part names that office software does not generate were found (%d); they are called by their ZIP entry number here, stay in the file, and can hold personal text", found)
	}
}
