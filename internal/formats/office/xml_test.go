package office

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

func noEdits(*xmlElement) error { return nil }

// TestUneditedTextIsByteIdentical: formatting quirks that a round trip through a
// real XML library would normalise must all survive when nothing asks for a change.
func TestUneditedTextIsByteIdentical(t *testing.T) {
	for _, doc := range []string{
		`<a/>`,
		`<?xml version="1.0"?>` + "\n" + `<a  b = "1"  c='2'  ><d  />text &amp; more<e></e ></a>`,
		"\xEF\xBB\xBF" + `<?xml version="1.0" encoding="UTF-8"?><a/>`,
		`<a b="&lt;&#65;&#x41;&quot;">&gt;<![CDATA[<raw & "stuff">]]></a>`,
		"<a>\r\n\t<b/>\r\n</a>\r\n",
		`<a xmlns="urn:x" xmlns:p="urn:y"><p:b p:c="1"/></a>`,
		`<a b="it's" c='say "hi"'/>`,
		`<a b=">"/>`,
		`<a>]] &gt; ] &lt;</a>`,
		`<ünï xmlns:é="urn:x"><é:b/></ünï>`,
	} {
		res, err := editXML(context.Background(), []byte(doc), noEdits)
		if err != nil {
			t.Errorf("%q: %v", doc, err)
			continue
		}
		if string(res.out) != doc {
			t.Errorf("changed:\n got %q\nwant %q", res.out, doc)
		}
	}
}

func TestCommentsAndInstructionsAreRemovedAndCounted(t *testing.T) {
	const doc = `<?xml version="1.0"?><!-- Test Banner --><?mso-application progid="Test"?><a><!-- <b w:author="x"/> --><c/><?pi?></a><!-- tail -->`
	res, err := editXML(context.Background(), []byte(doc), noEdits)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<?xml version="1.0"?><a><c/></a>`; string(res.out) != want {
		t.Errorf("got %q, want %q", res.out, want)
	}
	if res.comments != 3 || res.firstComment != " Test Banner " || res.instructions != 2 {
		t.Errorf("comments=%d first=%q instructions=%d", res.comments, res.firstComment, res.instructions)
	}
}

func TestMarkupInsideCommentsAndCDATAIsNotVisited(t *testing.T) {
	const doc = `<a><!-- <fake1/> --><![CDATA[<fake2/>]]><real/>&lt;fake3/&gt;<?pi <fake4/>?></a>`
	var seen []string
	_, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
		seen = append(seen, e.local)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, ","); got != "a,real" {
		t.Errorf("visited %s", got)
	}
}

func TestAttributeEdits(t *testing.T) {
	const doc = `<a keep1="1"  drop="x" keep2='2' set="old" tail="t"/>`
	res, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
		for i := range e.attrs {
			switch a := &e.attrs[i]; a.local {
			case "drop":
				e.dropAttr(a)
			case "set":
				e.setAttr(a, `<new & "q">`)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `<a keep1="1" keep2='2' set="&lt;new &amp; &quot;q&quot;&gt;" tail="t"/>`; string(res.out) != want {
		t.Errorf("got %s\nwant %s", res.out, want)
	}
}

func TestSubtreeDropAndContentReplace(t *testing.T) {
	const doc = `<a><gone x="1"><deep><gone/></deep>text</gone><kept/><gone/><swap>old<b>c</b></swap><swap/></a>`
	res, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
		switch e.local {
		case "gone":
			e.drop()
		case "swap":
			e.setContent("new & <safe>")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `<a><kept/><swap>new &amp; &lt;safe&gt;</swap><swap/></a>`; string(res.out) != want {
		t.Errorf("got %s\nwant %s", res.out, want)
	}
}

func TestNamespacesAreResolvedAndScoped(t *testing.T) {
	const doc = `<a xmlns="urn:one" xmlns:p="urn:two"><p:b q="1" p:c="2"><d xmlns="urn:three" xmlns:p="urn:four"><p:e/></d></p:b><f/></a>`
	type name struct{ space, local string }
	var got []name
	var attrs []name
	_, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
		got = append(got, name{e.space, e.local})
		for i := range e.attrs {
			if a := &e.attrs[i]; a.space != nsXMLNS {
				attrs = append(attrs, name{a.space, a.local})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []name{{"urn:one", "a"}, {"urn:two", "b"}, {"urn:three", "d"}, {"urn:four", "e"}, {"urn:one", "f"}}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("elements = %v, want %v", got, want)
		}
	}
	// An unprefixed attribute has no namespace, whatever the default namespace is.
	if len(attrs) != 2 || attrs[0] != (name{"", "q"}) || attrs[1] != (name{"urn:two", "c"}) {
		t.Errorf("attributes = %v", attrs)
	}
}

func TestMalformedXMLIsRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":                 "",
		"only whitespace":       "   ",
		"only a declaration":    `<?xml version="1.0"?>`,
		"unterminated tag":      `<a`,
		"unterminated attr":     `<a b="1`,
		"unterminated comment":  `<a><!-- x</a>`,
		"unterminated cdata":    `<a><![CDATA[x</a>`,
		"unterminated pi":       `<a><?x</a>`,
		"unclosed":              `<a><b></a>`,
		"crossed":               `<a><b></a></b>`,
		"stray end":             `</a>`,
		"end after root":        `<a/></a>`,
		"second root":           `<a/><b/>`,
		"text before root":      `x<a/>`,
		"text after root":       `<a/>x`,
		"cdata after root":      `<a/><![CDATA[x]]>`,
		"missing quotes":        `<a b=1/>`,
		"missing value":         `<a b/>`,
		"missing space":         `<a b="1"c="2"/>`,
		"duplicate attribute":   `<a b="1" b="2"/>`,
		"< in attribute":        `<a b="<"/>`,
		"stray slash":           `<a / b="1"/>`,
		"empty tag":             `<>`,
		"empty end tag":         `<a></>`,
		"end tag attributes":    `<a></a b="1">`,
		"undeclared element":    `<p:a/>`,
		"undeclared attribute":  `<a p:b="1"/>`,
		"late declaration":      `<a/><?xml version="1.0"?>`,
		"declaration not first": ` <?xml version="1.0"?><a/>`,
		"doctype after root":    `<a/><!DOCTYPE a>`,
		"unknown declaration":   `<!ELEMENT a><a/>`,
		"nul":                   "<a>\x00</a>",
		"invalid utf-8":         "<a>\xFF</a>",
		"odd utf-16":            "\xFF\xFE<\x00a",
		"utf-32":                "\xFF\xFE\x00\x00<\x00\x00\x00a\x00\x00\x00/\x00\x00\x00>\x00\x00\x00",
		"fake utf-16":           `<?xml version="1.0" encoding="UTF-16"?><a/>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := editXML(context.Background(), []byte(doc), noEdits)
			if !errors.Is(err, core.ErrMalformed) {
				t.Errorf("got %v, want ErrMalformed", err)
			}
		})
	}
}

func TestDangerousConstructsAreRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"internal subset": `<!DOCTYPE a [<!ENTITY e "secret">]><a b="&e;"/>`,
		"utf-7":           `<?xml version="1.0" encoding="UTF-7"?>+ADw-a/+AD4-`,
		"ebcdic":          `<?xml version="1.0" encoding="IBM037"?><a/>`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := editXML(context.Background(), []byte(doc), noEdits)
			if !errors.Is(err, core.ErrUnsupported) {
				t.Errorf("got %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestPlainDoctypeIsRemoved(t *testing.T) {
	// Old OpenOffice manifests carry one, and it can name a file on someone's disk.
	const doc = `<?xml version="1.0"?><!DOCTYPE manifest:manifest PUBLIC "-//OpenOffice.org//DTD Manifest 1.0//EN" "file:///C:/Users/test/Manifest.dtd"><a/>`
	res, err := editXML(context.Background(), []byte(doc), noEdits)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<?xml version="1.0"?><a/>`; string(res.out) != want {
		t.Errorf("got %s", res.out)
	}
	if res.instructions != 1 {
		t.Errorf("instructions = %d", res.instructions)
	}
}

func TestDeclaredEightBitEncodingsAreAccepted(t *testing.T) {
	doc := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><a b=\"\xE9\">\xE9</a>"
	res, err := editXML(context.Background(), []byte(doc), noEdits)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.out) != doc {
		t.Errorf("changed: %q", res.out)
	}
}

func TestDepthLimit(t *testing.T) {
	doc := strings.Repeat("<a>", maxXMLDepth+1)
	_, err := editXML(context.Background(), []byte(doc), noEdits)
	if !errors.Is(err, core.ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestManyAttributesStillChecked(t *testing.T) {
	var b strings.Builder
	b.WriteString("<a")
	for i := range 100 {
		b.WriteString(" n")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString(`="1"`)
	}
	ok := b.String() + "/>"
	if _, err := editXML(context.Background(), []byte(ok), noEdits); err != nil {
		t.Errorf("100 distinct attributes: %v", err)
	}
	dup := b.String() + ` nxx="2"/>`
	if _, err := editXML(context.Background(), []byte(dup), noEdits); !errors.Is(err, core.ErrMalformed) {
		t.Errorf("duplicate among many: got %v", err)
	}
}

func TestDecodeEntities(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "plain",
		"&amp;&lt;&gt;":    "&<>",
		"&quot;&apos;":     `"'`,
		"&#65;&#x41;":      "AA",
		"&#x1F600;":        "😀",
		"a&b":              "a&b",
		"a&unknown;b":      "a&unknown;b",
		"&#xFFFFFFFFFF;":   "&#xFFFFFFFFFF;",
		"&#;":              "&#;",
		"&":                "&",
		"&&amp;":           "&&",
		"&amp":             "&amp",
		"&waytoolongname;": "&waytoolongname;",
	} {
		if got := decodeEntities([]byte(in)); got != want {
			t.Errorf("decodeEntities(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeafText(t *testing.T) {
	for doc, want := range map[string]struct {
		text string
		leaf bool
	}{
		`<r><x>a &amp; b</x></r>`:           {"a & b", true},
		`<r><x><![CDATA[<a>]]>c</x></r>`:    {"<a>c", true},
		`<r><x>a<!-- no -->b<?p?>c</x></r>`: {"abc", true},
		`<r><x/></r>`:                       {"", true},
		`<r><x>a<y/>b</x></r>`:              {"", false},
	} {
		var text string
		var leaf bool
		_, err := editXML(context.Background(), []byte(doc), func(e *xmlElement) error {
			if e.local == "x" {
				text, leaf = e.leafText()
				e.drop()
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", doc, err)
			continue
		}
		if text != want.text || leaf != want.leaf {
			t.Errorf("%s: got %q, %v; want %q, %v", doc, text, leaf, want.text, want.leaf)
		}
	}
}
