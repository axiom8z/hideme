package office

import (
	"slices"
	"strings"
)

// markupPolicy says what a part's markup may hold beyond what its rules look at. A
// reader skips what it does not know, so markup outside the format can hold anything at
// all. A complete schema of each format is out of reach here, but three things can be
// told without one, and it is those that go:
//
//   - elements and attributes in a namespace the format does not define, and
//     declarations of such a namespace;
//   - text beside child elements, in formats whose elements hold either text or children;
//   - attributes with no prefix, on elements that only ever have prefixed ones.
//
// A part whose root element is in a namespace the format does not define is a vendor's
// own, and has its own rules, so it is left alone.
type markupPolicy struct {
	spaces []string // the namespace prefixes that are known
	// mixed reports whether elements of a namespace may hold text beside child elements.
	mixed func(space string) bool
	// plain reports whether an element may have an attribute with no prefix.
	plain func(space, local, attr string) bool
}

func (m *markupPolicy) known(uri string) bool {
	return slices.ContainsFunc(m.spaces, func(prefix string) bool { return strings.HasPrefix(uri, prefix) })
}

var (
	// ooxmlPolicy covers the namespaces of the standard and of Microsoft's extensions,
	// whichever the reader may understand. No element in these formats mixes text with
	// children. Wordprocessing attributes are all prefixed, and the two package formats
	// are small enough to list. Spreadsheet and drawing attributes have no prefix and
	// their names run to thousands, so an unknown one goes unnoticed, as the handler's
	// limitations say.
	ooxmlPolicy = &markupPolicy{
		spaces: []string{"http://schemas.openxmlformats.org/", "http://purl.oclc.org/ooxml/", "http://schemas.microsoft.com/",
			"urn:schemas-microsoft-com:", "http://www.w3.org/", "http://purl.org/dc/"},
		mixed: func(string) bool { return false },
		plain: func(space, local, attr string) bool {
			switch space {
			case nsWord, nsWordStrict:
				return false
			case nsPackageRels:
				return local == "Relationship" && (attr == "Id" || attr == "Type" || attr == "Target" || attr == "TargetMode")
			case nsContentTypes:
				switch local {
				case "Default":
					return attr == "Extension" || attr == "ContentType"
				case "Override":
					return attr == "PartName" || attr == "ContentType"
				}
				return false
			}
			return true
		},
	}
	// odfPolicy covers OASIS's namespaces, what OpenOffice and LibreOffice add, and what
	// the standard borrows. Only paragraphs and what lies in them mix text with children.
	// Every attribute of an OpenDocument element is prefixed.
	odfPolicy = &markupPolicy{
		spaces: []string{"urn:oasis:names:tc:opendocument:", "http://docs.oasis-open.org/", "http://openoffice.org/",
			"urn:org:documentfoundation:", "urn:openoffice:", "http://www.w3.org/", "http://purl.org/dc/", "http://schemas.microsoft.com/office/"},
		mixed: func(space string) bool { return space == nsText },
		plain: func(space, _, _ string) bool {
			return !strings.HasPrefix(space, "urn:oasis:names:tc:opendocument:")
		},
	}
)

// compatibilityLists are the markup compatibility attributes that list prefixes. A
// reader refuses a document if one of them names a prefix with no declaration, so a
// namespace that goes has to come off these too.
var compatibilityLists = map[string]bool{
	"Ignorable": true, "ProcessContent": true, "MustUnderstand": true, "PreserveElements": true, "PreserveAttributes": true,
}

// knownInList returns what is left of a compatibility list once namespaces that do not
// stay are gone, and whether that is all of it. An entry can be a prefix, or a prefix and a name.
func (ed *xmlEditor) knownInList(list string) (kept []string, allKnown bool) {
	allKnown = true
	for _, entry := range strings.Fields(list) {
		prefix, _, _ := strings.Cut(entry, ":")
		uri, ok := ed.lookup([]byte(prefix))
		if ok && prefix != "" && ed.policy.known(uri) {
			kept = append(kept, entry)
			continue
		}
		allKnown = false
	}
	return kept, allKnown
}

// applyPolicy removes from a start tag what the policy has no place for, after the
// visitor has seen it, so that what the rules know is reported by name. It reports true
// if the element itself has to go.
func (ed *xmlEditor) applyPolicy(el *xmlElement) bool {
	data := ed.scan.data
	policy := ed.policy
	if !policy.known(el.space) {
		ed.res.foreign++
		return true
	}
	for i := range el.attrs {
		a := &el.attrs[i]
		switch {
		case a.space == nsXMLNS:
			if !policy.known(decodeEntities(data[a.valStart:a.valEnd])) {
				el.dropAttr(a)
				ed.res.foreign++
			}
		case a.space == "":
			if el.space == nsMarkup && el.local == "Choice" && a.local == "Requires" {
				if _, allKnown := ed.knownInList(decodeEntities(data[a.valStart:a.valEnd])); !allKnown {
					ed.res.foreign++
					return true
				}
			} else if !policy.plain(el.space, el.local, a.local) {
				el.dropAttr(a)
				ed.res.foreign++
			}
		case !policy.known(a.space):
			el.dropAttr(a)
			ed.res.foreign++
		case a.space == nsMarkup && compatibilityLists[a.local]:
			kept, allKnown := ed.knownInList(decodeEntities(data[a.valStart:a.valEnd]))
			switch {
			case allKnown:
			case len(kept) == 0:
				el.dropAttr(a)
			default:
				el.setAttr(a, strings.Join(kept, " "))
			}
		}
	}
	return false
}

// strayText reports whether non-blank text at the current token lies beside child
// elements, in an element whose format does not mix them. Text is only sure to be alone
// if the element ends before anything else starts, so it looks ahead, and remembers the
// answer, so that a run of text and comments is read once.
func (ed *xmlEditor) strayText(body []byte) bool {
	if isBlank(body) {
		return false
	}
	top := &ed.stack[len(ed.stack)-1]
	if ed.policy.mixed(top.space) {
		return false
	}
	if top.hasChild {
		return true
	}
	if ed.scan.tok.start < ed.textRunEnd {
		return ed.textRunStray
	}
	sc := xmlScanner{data: ed.scan.data, pos: ed.scan.tok.end}
	for {
		more, err := sc.next()
		if err != nil || !more {
			return false // the damage is reported when the walk reaches it
		}
		switch sc.tok.kind {
		case tokenStart:
			ed.textRunEnd, ed.textRunStray = sc.tok.start, true
			return true
		case tokenEnd:
			ed.textRunEnd, ed.textRunStray = sc.tok.start, false
			return false
		}
	}
}
