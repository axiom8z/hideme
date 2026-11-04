package office

import (
	"strconv"
	"strings"

	"github.com/axiom8z/hideme/internal/core"
)

// hiddenKind is something a reader of the document does not see but anyone can recover
// from the file. All of it is the user's content, so none of it is removed, but people
// often believe it is gone, so each kind found earns one warning for the whole file.
// A hash goes here too: it is not hidden on purpose, but it is just as forgotten.
type hiddenKind int

const (
	hiddenNotes hiddenKind = iota
	hiddenSlides
	hiddenSheets
	hiddenVerySheets
	hiddenRowsColumns
	hiddenNames
	hiddenText
	hiddenStyles
	hiddenObjects
	hiddenPasswords
	hiddenKinds
)

// None of these may ever quote the content they are about, since warnings reach logs.
var hiddenWarnings = [hiddenKinds]string{
	hiddenNotes:       "speaker notes stay in the file; they are content, so review them before sharing",
	hiddenSlides:      "hidden slides stay in the file; they are content, so review them before sharing",
	hiddenSheets:      "hidden sheets or tables stay in the file; they are content, so unhide and review them before sharing",
	hiddenVerySheets:  "very hidden sheets stay in the file, and Excel's Unhide menu does not list them; they are content, so review them before sharing",
	hiddenRowsColumns: "hidden rows or columns stay in the file; they are content, so unhide and review them before sharing",
	hiddenNames:       "hidden named ranges stay in the file and can hold text or paths; they are content, so review them before sharing",
	hiddenText:        "hidden text stays in the file; it is content, so show it and review it before sharing",
	hiddenStyles:      "a style marks text as hidden, and any text using it stays in the file; it is content, so review it before sharing",
	hiddenObjects:     "hidden drawing objects stay in the file; they are content, so show and review them before sharing",
	hiddenPasswords:   "a password hash for document or sheet protection stays in the file; it can be attacked offline, so do not reuse that password",
}

// hiddenContent records what one file holds. Parts are edited one at a time, and this
// is shared between them, so that a deck with 100 notes pages gives one warning.
type hiddenContent [hiddenKinds]bool

func (h *hiddenContent) warn(rep *core.Report) {
	for kind, found := range h {
		if found {
			rep.Warn("%s", hiddenWarnings[kind])
		}
	}
}

// isOn reads an attribute of schema type xsd:boolean or ST_OnOff.
func isOn(value string) bool {
	switch value {
	case "1", "true", "on":
		return true
	}
	return false
}

// holdsPassword reports whether an element has a non-empty attribute of one of these
// names. Protection without a password writes the element and none of them.
func holdsPassword(e *xmlElement, names ...string) bool {
	for i := range e.attrs {
		a := &e.attrs[i]
		if a.space == nsXMLNS || a.valStart == a.valEnd {
			continue
		}
		for _, name := range names {
			if a.local == name {
				return true
			}
		}
	}
	return false
}

// wordVanish handles a w:vanish, which hides text only in some positions. On a
// paragraph mark or in the record of old formatting it hides no text at all, and Word
// writes those often, so a run's own property waits for text to appear in that run.
func (ed *partEditor) wordVanish(e *xmlElement) {
	if value, ok := e.attr(e.space, "val"); ok && !isOn(value) {
		return
	}
	if local, _, ok := e.ancestor(0); !ok || local != "rPr" {
		return
	}
	owner, start, ok := e.ancestor(1)
	if !ok {
		return
	}
	switch owner {
	case "r":
		ed.hiddenRun = start
	case "style", "rPrDefault":
		ed.hidden[hiddenStyles] = true
	}
}

// hiddenRunText is called for text in a run.
func (ed *partEditor) hiddenRunText(e *xmlElement) {
	if ed.hiddenRun == 0 {
		return
	}
	if local, start, ok := e.ancestor(0); !ok || local != "r" || start != ed.hiddenRun {
		return
	}
	if text, leaf := e.leafText(); leaf && strings.TrimSpace(text) != "" {
		ed.hidden[hiddenText] = true
	}
}

// sheetVisibility covers a sheet's state and a row or column hidden either by name or
// by having no size at all, which looks the same to anyone reading the sheet.
func (ed *partEditor) sheetVisibility(e *xmlElement) {
	switch e.local {
	case "sheet":
		switch state, _ := e.attr("", "state"); state {
		case "hidden":
			ed.hidden[hiddenSheets] = true
		case "veryHidden":
			ed.hidden[hiddenVerySheets] = true
		}
	case "row", "col":
		size := "ht"
		if e.local == "col" {
			size = "width"
		}
		hidden, _ := e.attr("", "hidden")
		value, sized := e.attr("", size)
		if zero, err := strconv.ParseFloat(value, 64); isOn(hidden) || (sized && err == nil && zero == 0) {
			ed.hidden[hiddenRowsColumns] = true
		}
	case "definedName":
		// Excel hides names of its own for filters and newer functions.
		hidden, _ := e.attr("", "hidden")
		name, _ := e.attr("", "name")
		if isOn(hidden) && !strings.HasPrefix(name, "_xl") {
			ed.hidden[hiddenNames] = true
		}
	}
}

// notesText looks for words typed on a notes page. A notes page always exists to hold
// a slide number, which is a field, so an empty one has nothing to warn about.
func (ed *partEditor) notesText(e *xmlElement) {
	if local, _, ok := e.ancestor(0); !ok || local != "r" {
		return
	}
	if text, leaf := e.leafText(); leaf && strings.TrimSpace(text) != "" {
		ed.hidden[hiddenNotes] = true
	}
}

// odfNotes looks for text on a slide's notes page. The notes of a master page hold a
// placeholder, so they do not count. Nested notes are covered by the outermost, which
// keeps the total work linear in the part's size.
func (ed *odfEditor) odfNotes(e *xmlElement) {
	if ed.hidden[hiddenNotes] || e.offset() < ed.notesEnd {
		return
	}
	if _, parent := e.parent(); parent != "page" {
		return
	}
	if end, ok := e.subtreeEnd(); ok {
		ed.notesEnd = end
	}
	if e.subtreeText(1) != "" {
		ed.hidden[hiddenNotes] = true
	}
}

// odfAttr covers the hidden forms ODF spells as attributes, which are as likely to sit
// on a style as on the object itself.
func (ed *odfEditor) odfAttr(e *xmlElement, a *xmlAttr) {
	if a.local == "protection-key" && a.valStart != a.valEnd {
		ed.hidden[hiddenPasswords] = true
		return
	}
	switch a.space {
	case nsTable:
		switch {
		case a.local == "display" && e.value(a) == "false":
			ed.hidden[hiddenSheets] = true
		case a.local == "visibility" && e.value(a) != "visible":
			ed.hidden[hiddenRowsColumns] = true
		}
	case nsText:
		if value := e.value(a); a.local == "display" && (value == "none" || value == "condition") {
			if e.local == "text-properties" {
				ed.hidden[hiddenStyles] = true
			} else {
				ed.hidden[hiddenText] = true
			}
		}
	case nsPresentation:
		if a.local == "visibility" && e.value(a) == "hidden" {
			ed.hidden[hiddenSlides] = true
		}
	case nsDraw:
		if a.local == "display" && e.value(a) == "none" {
			ed.hidden[hiddenObjects] = true
		}
	}
}
