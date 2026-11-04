package svg

import (
	"regexp"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// marker stands for any text from a file. Field and Where are printed even by clean,
// which promises to print no metadata, so they must never hold any of it. Value is
// hidden there. Errors may quote it, since the command masks what is in quotes.
const marker = "ZZMARKER"

var quotedInError = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func checkNoMarker(t *testing.T, rep *core.Report) {
	t.Helper()
	for _, f := range rep.Findings {
		if strings.Contains(f.Field, marker) || strings.Contains(f.Where, marker) || strings.Contains(f.Reason, marker) {
			t.Errorf("file text reached a printed part of a finding: %+v", f)
		}
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, marker) {
			t.Errorf("file text reached a warning: %q", w)
		}
	}
}

func TestFileTextStaysOutOfPrintedFields(t *testing.T) {
	const m = marker
	foreign := `http://example.com/` + m
	image := b64(secretImage)
	for name, data := range map[string]string{
		"foreign element":                        svgDoc(`<` + m + ` xmlns="` + foreign + `">x</` + m + `>`),
		"foreign prefixed":                       svgDoc(`<` + m + `:a xmlns:` + m + `="` + foreign + `"/>`),
		"unknown element":                        svgDoc(`<` + m + `/>`),
		"unknown attribute":                      svgDoc(`<rect ` + m + `="1"/>`),
		"unknown data attribute":                 svgDoc(`<rect data-` + m + `="1"/>`),
		"xlink unknown attribute":                svgDoc(`<rect xlink:` + m + `="1"/>`),
		"editor attribute":                       svgDoc(`<rect xmlns:` + m + `="` + foreign + `" ` + m + `:x="1"/>`),
		"editor namespace on a prefixed element": `<` + m + `:svg xmlns:` + m + `="http://www.w3.org/2000/svg" xmlns:b="` + foreign + `"/>`,
		"inkscape label on a prefixed element":   `<` + m + `:svg xmlns:` + m + `="http://www.w3.org/2000/svg" xmlns:inkscape="` + nsInkscape + `" inkscape:label="x"/>`,
		"hidden text in a prefixed group":        `<svg xmlns="` + nsSVG + `" xmlns:` + m + `="` + nsSVG + `"><` + m + `:g>hidden</` + m + `:g></svg>`,
		"title in a prefixed group":              `<svg xmlns="` + nsSVG + `" xmlns:` + m + `="` + nsSVG + `"><` + m + `:g><title>Logo</title></` + m + `:g></svg>`,
		"banner in a prefixed group":             `<svg xmlns="` + nsSVG + `" xmlns:` + m + `="` + nsSVG + `"><` + m + `:g><desc>Created with x</desc></` + m + `:g></svg>`,
		"link through a prefix":                  `<svg xmlns="` + nsSVG + `" xmlns:` + m + `="` + nsXLink + `"><image ` + m + `:href="file:///a"/></svg>`,
		"processing instruction":                 svgDoc(`<?` + m + ` x?>`),
		"stylesheet title":                       `<?xml-stylesheet href="a" title="` + m + `"?><svg/>`,
		"RDF property":                           svgDoc(`<metadata><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:` + m + `="` + foreign + `" ` + m + `:p="v"><` + m + `:q>v</` + m + `:q></rdf:Description></rdf:RDF></metadata>`),
		"RDF in a foreign element":               svgDoc(`<` + m + ` xmlns="` + foreign + `"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description/></rdf:RDF></` + m + `>`),
		"data URI parameter":                     svgDoc(`<image href="data:image/png;name=` + m + `;base64,` + image + `"/>`),
		"XHTML element":                          svgDoc(`<foreignObject><` + m + ` xmlns="` + nsXHTML + `" ` + m + `="1"/></foreignObject>`),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			checkNoMarker(t, rep)
			strict, rep := clean(t, data, core.Options{Strict: true})
			_ = strict
			checkNoMarker(t, rep)
		})
	}
}

func TestFileTextInErrorsIsQuoted(t *testing.T) {
	const m = marker
	for name, data := range map[string]string{
		"encoding":        `<?xml version="1.0" encoding="` + m + `"?><svg/>`,
		"version":         `<?xml version="` + m + `"?><svg/>`,
		"declaration":     `<?xml version="1.0" ` + m + `="1"?><svg/>`,
		"root namespace":  `<svg xmlns="http://example.com/` + m + `"/>`,
		"unclosed":        `<svg><` + m + `>`,
		"mismatched":      `<svg><g></` + m + `></svg>`,
		"undeclared":      `<svg><` + m + `:a/></svg>`,
		"undeclared attr": `<svg><g ` + m + `:a="1"/></svg>`,
		"duplicate":       `<svg><g ` + m + `="1" ` + m + `="2"/></svg>`,
		"empty prefix":    `<svg xmlns:` + m + `=""/>`,
		"element name":    `<svg><` + m + `:a:b/></svg>`,
		"attribute name":  `<svg><g ` + m + `:a:b="1"/></svg>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := analyse(t.Context(), core.BytesSource([]byte(data)), core.Options{})
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(quotedInError.ReplaceAllString(err.Error(), ""), m) {
				t.Errorf("unquoted file text in %q", err)
			}
		})
	}
}
