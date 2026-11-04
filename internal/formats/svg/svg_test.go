package svg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// fakeImage stands in for the JPEG and PNG handlers, which this package must not
// import. A fake image is its magic, then pixels, then an optional metadata tail
// that cleaning removes.
type fakeImage struct{}

const (
	fakeMagic   = "FAKEIMG\n"
	fakeTail    = "\nSECRET="
	brokenMagic = "BROKENIMG\n"
)

func init() { core.Register(fakeImage{}) }

func (fakeImage) Format() core.Format {
	return core.Format{ID: "fakeimage", Name: "Fake image", Family: core.FamilyImage, Extensions: []string{".fakeimg"}}
}

func (fakeImage) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte(fakeMagic)) || bytes.HasPrefix(head, []byte(brokenMagic))
}

func (fakeImage) clean(src core.Source) ([]byte, *core.Report, error) {
	data, err := core.ReadAll(src)
	if err != nil {
		return nil, nil, err
	}
	if bytes.HasPrefix(data, []byte(brokenMagic)) {
		return nil, nil, core.Malformedf("fake: broken image")
	}
	rep := &core.Report{Format: "fakeimage"}
	body, secret, found := bytes.Cut(data, []byte(fakeTail))
	if found {
		rep.Add(core.Finding{Category: core.CatLocation, Risk: core.RiskHigh, Field: "Fake secret", Value: string(secret), Action: core.ActionRemove})
	}
	rep.Add(core.Finding{Category: core.CatTechnical, Risk: core.RiskNone, Field: "Fake orientation", Action: core.ActionRetain, Reason: "keeps the picture upright"})
	return body, rep, nil
}

func (f fakeImage) Inspect(_ context.Context, src core.Source, _ core.Options) (*core.Report, error) {
	_, rep, err := f.clean(src)
	return rep, err
}

func (f fakeImage) Clean(_ context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	out, rep, err := f.clean(src)
	if err != nil {
		return nil, err
	}
	_, err = dst.Write(out)
	return rep, err
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

var (
	secretImage = fakeMagic + "PIXELS" + fakeTail + "Eiffel Tower 48.8584, 2.2945"
	cleanImage  = fakeMagic + "PIXELS"
)

// inkscapeFile follows what Inkscape 1.x writes, with personal values planted where
// it really puts them.
var inkscapeFile = `<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<!-- Created with Inkscape (http://www.inkscape.org/) -->

<svg
   width="210mm"
   height="297mm"
   viewBox="0 0 210 297"
   version="1.1"
   id="svg5"
   inkscape:version="1.2.2 (732a01da63, 2022-12-09)"
   sodipodi:docname="secret-plan.svg"
   sodipodi:docbase="/home/testauthor/Documents"
   inkscape:export-filename="C:\Users\Test Author\Documents\secret-plan.png"
   inkscape:export-xdpi="96"
   inkscape:export-ydpi="96"
   xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"
   xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
   xmlns:xlink="http://www.w3.org/1999/xlink"
   xmlns="http://www.w3.org/2000/svg"
   xmlns:svg="http://www.w3.org/2000/svg"
   xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
   xmlns:cc="http://creativecommons.org/ns#"
   xmlns:dc="http://purl.org/dc/elements/1.1/">
  <sodipodi:namedview
     id="namedview7"
     pagecolor="#ffffff"
     bordercolor="#666666"
     borderopacity="1.0"
     inkscape:pageopacity="0.0"
     inkscape:pagecheckerboard="0"
     inkscape:document-units="mm"
     showgrid="false"
     inkscape:zoom="0.74"
     inkscape:cx="396.8"
     inkscape:window-width="1920"
     inkscape:window-height="1017"
     inkscape:current-layer="layer1">
    <sodipodi:guide position="10,10" orientation="0,1" id="guide1"/>
    <inkscape:grid type="xygrid" id="grid1"/>
  </sodipodi:namedview>
  <defs
     id="defs2">
    <inkscape:path-effect
       effect="spiro"
       id="path-effect1"
       is_visible="true"
       lpeversion="1"/>
    <linearGradient
       inkscape:collect="always"
       id="linearGradient1">
      <stop
         style="stop-color:#000000;stop-opacity:1"
         offset="0"
         id="stop1"/>
    </linearGradient>
  </defs>
  <metadata
     id="metadata5">
    <rdf:RDF>
      <cc:Work
         rdf:about="">
        <dc:format>image/svg+xml</dc:format>
        <dc:type
           rdf:resource="http://purl.org/dc/dcmitype/StillImage"/>
        <dc:title>Test Metadata Title</dc:title>
        <dc:date>2001-02-03</dc:date>
        <dc:creator>
          <cc:Agent>
            <dc:title>Test Author</dc:title>
          </cc:Agent>
        </dc:creator>
        <dc:rights>
          <cc:Agent>
            <dc:title>Test Rights Holder</dc:title>
          </cc:Agent>
        </dc:rights>
        <dc:publisher>
          <cc:Agent>
            <dc:title>Test Publisher</dc:title>
          </cc:Agent>
        </dc:publisher>
        <dc:description>Test Metadata Description</dc:description>
        <cc:license
           rdf:resource="http://creativecommons.org/licenses/by/4.0/"/>
      </cc:Work>
    </rdf:RDF>
  </metadata>
  <g
     inkscape:label="Test Layer Name"
     inkscape:groupmode="layer"
     id="layer1"
     transform="translate(1.5,2.5)">
    <path
       style="fill:#ff0000;stroke:none"
       d="M 10,10 H 20 V 20 Z"
       id="path1"
       sodipodi:nodetypes="cccc"
       inkscape:connector-curvature="0"/>
    <text
       xml:space="preserve"
       style="font-size:4px"
       x="10"
       y="30"
       id="text1"><tspan
         sodipodi:role="line"
         id="tspan1"
         x="10"
         y="30">Visible Text</tspan></text>
    <rect
       style="fill:url(#linearGradient1)"
       id="rect1"
       width="5"
       height="5"
       x="1"
       y="1"
       ry="0.5"/>
  </g>
</svg>
`

var inkscapeSecrets = asBytes(
	"inkscape", "sodipodi", "secret-plan", "testauthor", "Test Author", "Test Metadata", "Test Rights",
	"Test Publisher", "Test Layer", "2001-02-03", "rdf:", "cc:", "dc:", "xmlns:rdf", "creativecommons",
	"window-width", "namedview", "1.2.2",
)

var inkscapeKept = asBytes(
	`viewBox="0 0 210 297"`, `d="M 10,10 H 20 V 20 Z"`, `style="fill:#ff0000;stroke:none"`,
	`<?xml version="1.0" encoding="UTF-8" standalone="no"?>`, ">Visible Text</tspan></text>",
	`xmlns:xlink="http://www.w3.org/1999/xlink"`, `xmlns:svg="http://www.w3.org/2000/svg"`,
	`style="fill:url(#linearGradient1)"`, `transform="translate(1.5,2.5)"`,
)

// illustratorFile follows Illustrator's SVG export with "preserve editing
// capabilities" on, including the entity-valued namespaces and the private data.
const illustratorFile = `<?xml version="1.0" encoding="utf-8"?>
<!-- Generator: Adobe Illustrator 24.0.1, SVG Export Plug-In . SVG Version: 6.00 Build 0)  -->
<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd" [
	<!ENTITY ns_extend "http://ns.adobe.com/Extensibility/1.0/">
	<!ENTITY ns_ai "http://ns.adobe.com/AdobeIllustrator/10.0/">
	<!ENTITY ns_graphs "http://ns.adobe.com/Graphs/1.0/">
	<!ENTITY ns_vars "http://ns.adobe.com/Variables/1.0/">
	<!ENTITY ns_imrep "http://ns.adobe.com/ImageReplacement/1.0/">
	<!ENTITY ns_sfw "http://ns.adobe.com/SaveForWeb/1.0/">
	<!ENTITY ns_custom "http://ns.adobe.com/GenericCustomNamespace/1.0/">
	<!ENTITY ns_adobe_xpath "http://ns.adobe.com/XPath/1.0/">
]>
<svg version="1.1" id="Layer_1" xmlns:x="&ns_extend;" xmlns:i="&ns_ai;" xmlns:graph="&ns_graphs;"
	 xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" x="0px" y="0px" viewBox="0 0 100 100"
	 style="enable-background:new 0 0 100 100;" xml:space="preserve">
<style type="text/css">
	.st0{fill:#FF0000;}
</style>
<metadata>
	<sfw  xmlns="&ns_sfw;">
		<slices></slices>
		<sliceSourceBounds  bottomLeftOrigin="true" height="100" width="100" x="0" y="0"></sliceSourceBounds>
	</sfw>
	<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Adobe XMP Core 5.6">
		<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
			<rdf:Description rdf:about="" xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:CreatorTool="Test Illustrator Tool"
				xmlns:dc="http://purl.org/dc/elements/1.1/">
				<dc:creator><rdf:Seq><rdf:li>Test Author</rdf:li></rdf:Seq></dc:creator>
			</rdf:Description>
		</rdf:RDF>
	</x:xmpmeta>
</metadata>
<switch>
	<foreignObject requiredExtensions="&ns_ai;" x="0" y="0" width="1" height="1">
		<i:pgfRef  xlink:href="#adobe_illustrator_pgf">
		</i:pgfRef>
	</foreignObject>
	<g i:extraneous="self">
		<g id="Layer_2" i:layer="yes" i:dimmedPercent="50" i:rgbTrio="#4F008000FFFF">
			<rect x="10" y="10" class="st0" width="30" height="30"/>
			<text transform="matrix(1 0 0 1 10 60)" class="st0">Visible &amp; Text</text>
		</g>
	</g>
</switch>
<i:pgf  id="adobe_illustrator_pgf">
	<![CDATA[
	eJzsTest+Illustrator+Private+Data+Test+Author+C:/Users/TestAuthor/secret.ai
	]]>
</i:pgf>
</svg>
`

// sketchFile follows Sketch's export, and adds the Figma and Affinity namespaces.
const sketchFile = `<?xml version="1.0" encoding="UTF-8"?>
<svg width="24px" height="24px" viewBox="0 0 24 24" version="1.1"
  xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"
  xmlns:sketch="http://www.bohemiancoding.com/sketch/ns"
  xmlns:figma="http://www.figma.com/figma/ns"
  xmlns:serif="http://www.serif.com/">
    <!-- Generator: Sketch 52.6 (67491) - http://www.bohemiancoding.com/sketch -->
    <title>secret-icon.svg</title>
    <desc>Created with Sketch.</desc>
    <g id="Page-1" stroke="none" fill="none" sketch:type="MSPage" serif:id="Test Serif Name" figma:name="Test Figma Name">
        <sketch:artboard name="Test Artboard"/>
        <circle cx="12" cy="12" r="10" fill="#000"/>
    </g>
</svg>
`

func asBytes(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

func svgDoc(body string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">` + body + `</svg>`
}

func TestContract(t *testing.T) {
	imageBody := `<image xlink:href="data:image/png;base64,` + b64(secretImage) + `" width="1" height="1"/>`
	coretest.Contract(t, handler{}, []coretest.Case{
		{
			Name: "Inkscape", Data: []byte(inkscapeFile),
			WantFields: []string{
				"XML comment", "Editor version", "Document name", "Document folder", "Export file name", "Editor namespace",
				"Editor element", "Creator", "Title", "Rights", "Publisher", "Description", "Layer or object label", "Editor attributes",
			},
			MustNotContain: inkscapeSecrets, MustContain: inkscapeKept,
		},
		{
			Name: "Illustrator", Data: []byte(illustratorFile),
			WantFields:     []string{"XML comment", "Editor namespace", "Editor element", "Creator", "Creator tool", "Editor attributes"},
			MustNotContain: asBytes("Test Author", "Adobe Illustrator 24", "TestAuthor", "Illustrator+Private", "i:pgf", "xmlns:i=", "xmlns:x=", "xmlns:graph=", "i:layer", "i:extraneous", "xmpmeta", "sliceSourceBounds"),
			MustContain:    asBytes(`<!ENTITY ns_ai "http://ns.adobe.com/AdobeIllustrator/10.0/">`, `requiredExtensions="&ns_ai;"`, `class="st0"`, "Visible &amp; Text", ".st0{fill:#FF0000;}"),
		},
		{
			Name: "Sketch, Figma and Affinity", Data: []byte(sketchFile),
			WantFields:     []string{"XML comment", "Title", "Description", "Editor namespace", "Editor element", "Editor attributes"},
			MustNotContain: asBytes("Sketch", "sketch", "secret-icon", "figma", "serif", "Test Artboard"),
			MustContain:    asBytes(`<circle cx="12" cy="12" r="10" fill="#000"/>`, `id="Page-1"`),
		},
		{
			Name: "retained title", Data: []byte(svgDoc(`<title>A red circle</title><desc>A circle, filled red</desc><circle r="1"/>`)),
			WantRetained: []string{"Title", "Description"},
			MustContain:  asBytes("<title>A red circle</title>", "<desc>A circle, filled red</desc>"),
		},
		{
			Name: "embedded raster", Data: []byte(svgDoc(imageBody)),
			WantFields: []string{"Fake secret"}, WantRetained: []string{"Fake orientation"},
			MustNotContain: asBytes(b64(secretImage)), MustContain: asBytes(b64(cleanImage)),
		},
		{
			Name: "nothing to remove", Data: []byte(svgDoc(`<circle r="1"/>`)),
			MustContain: asBytes(svgDoc(`<circle r="1"/>`)),
		},
		{
			Name: "bare file without any namespace", Data: []byte(`<svg width="1"><metadata>Test Secret</metadata><rect/></svg>`),
			WantFields: []string{"Metadata element"}, MustNotContain: asBytes("Test Secret"), MustContain: asBytes(`<svg width="1"><rect/></svg>`),
		},
	})
}

func clean(t *testing.T, data string, opts core.Options) (string, *core.Report) {
	t.Helper()
	var out bytes.Buffer
	rep, err := handler{}.Clean(context.Background(), core.BytesSource([]byte(data)), &out, opts)
	if err != nil {
		t.Fatalf("Clean: %v\ninput: %.300s", err, data)
	}
	return out.String(), rep
}

func wantError(t *testing.T, data string, want error) {
	t.Helper()
	if _, err := (handler{}).Inspect(context.Background(), core.BytesSource([]byte(data)), core.Options{}); !errors.Is(err, want) {
		t.Errorf("Inspect: got %v, want %v, input: %.200q", err, want, data)
	}
	if _, err := (handler{}).Clean(context.Background(), core.BytesSource([]byte(data)), &bytes.Buffer{}, core.Options{}); !errors.Is(err, want) {
		t.Errorf("Clean: got %v, want %v, input: %.200q", err, want, data)
	}
}

func hasField(rep *core.Report, field string, action core.Action) bool {
	return slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return f.Field == field && f.Action == action })
}

// illustratorEntities are what the fixture's DOCTYPE declares, written out here
// rather than read through parseEntities so the check stays independent of it.
var illustratorEntities = map[string]string{
	"ns_extend":      "http://ns.adobe.com/Extensibility/1.0/",
	"ns_ai":          "http://ns.adobe.com/AdobeIllustrator/10.0/",
	"ns_graphs":      "http://ns.adobe.com/Graphs/1.0/",
	"ns_vars":        "http://ns.adobe.com/Variables/1.0/",
	"ns_imrep":       "http://ns.adobe.com/ImageReplacement/1.0/",
	"ns_sfw":         "http://ns.adobe.com/SaveForWeb/1.0/",
	"ns_custom":      "http://ns.adobe.com/GenericCustomNamespace/1.0/",
	"ns_adobe_xpath": "http://ns.adobe.com/XPath/1.0/",
}

func newDecoder(data string) *xml.Decoder {
	dec := xml.NewDecoder(strings.NewReader(data))
	dec.Entity = illustratorEntities
	return dec
}

var renderingNamespaces = map[string]bool{
	"": true, nsSVG: true, nsXLink: true, nsXML: true, "xml": true,
}

// renderedTokens lists what a renderer acts on, using encoding/xml, which shares
// nothing with the scanner. Cleaning must leave this list identical. Whitespace
// between elements is not part of it, because a removed element leaves its
// surrounding indentation behind.
func renderedTokens(t *testing.T, data string) []string {
	t.Helper()
	dec := newDecoder(data)
	var out []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("encoding/xml rejects the document: %v\n%s", err, data)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if !renderingNamespaces[tok.Name.Space] || tok.Name.Local == "metadata" {
				if err := dec.Skip(); err != nil {
					t.Fatalf("encoding/xml rejects the document: %v", err)
				}
				continue
			}
			var attrs []string
			for _, a := range tok.Attr {
				switch {
				case a.Name.Space == "xmlns" && !renderingNamespaces[a.Value]:
				case a.Name.Space == "" && a.Name.Local == "xmlns" && !renderingNamespaces[a.Value]:
				case a.Name.Space != "xmlns" && !renderingNamespaces[a.Name.Space]:
				default:
					attrs = append(attrs, a.Name.Space+"|"+a.Name.Local+"="+a.Value)
				}
			}
			slices.Sort(attrs)
			out = append(out, fmt.Sprintf("start %s|%s %q", tok.Name.Space, tok.Name.Local, attrs))
		case xml.EndElement:
			out = append(out, fmt.Sprintf("end %s|%s", tok.Name.Space, tok.Name.Local))
		case xml.CharData:
			if s := strings.TrimSpace(string(tok)); s != "" {
				out = append(out, "text "+s)
			}
		case xml.ProcInst:
			if tok.Target != "xml" && tok.Target != "xml-stylesheet" {
				continue
			}
			out = append(out, "instruction "+tok.Target+" "+string(tok.Inst))
		case xml.Directive:
			out = append(out, "directive "+string(tok))
		}
	}
}

func TestRenderedContentIsUnchanged(t *testing.T) {
	const keptOnly = `<?xml version="1.0"?><?xml-stylesheet href="a.css" type="text/css"?>
<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd">
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><!-- a -->
<defs><style><![CDATA[ .a > .b { fill: red; } ]]></style><linearGradient id="g"/></defs>
<use xlink:href="#g"/><text xml:space="preserve"> a &lt; b &#65; </text></svg>`
	for name, input := range map[string]string{
		"Inkscape": inkscapeFile, "Illustrator": illustratorFile, "Sketch": sketchWithoutRootText(), "kept only": keptOnly,
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := clean(t, input, core.Options{})
			want, got := renderedTokens(t, input), renderedTokens(t, out)
			if !slices.Equal(want, got) {
				t.Errorf("rendered tokens changed\n before: %q\n after:  %q", want, got)
			}
		})
	}
}

// sketchWithoutRootText drops the elements that are meant to go, since they would
// otherwise count as rendered tokens lost.
func sketchWithoutRootText() string {
	s := strings.Replace(sketchFile, "<title>secret-icon.svg</title>", "", 1)
	return strings.Replace(s, "<desc>Created with Sketch.</desc>", "", 1)
}

func TestExactOutput(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"attributes go along with the whitespace before them": {
			"<svg xmlns=\"http://www.w3.org/2000/svg\"\n  xmlns:a=\"urn:a\"\n  width=\"1\"\n  a:b=\"c\"\n  height='2'\n  ><g/></svg>",
			"<svg xmlns=\"http://www.w3.org/2000/svg\"\n  width=\"1\"\n  height='2'\n  ><g/></svg>",
		},
		"a removed element leaves its surroundings alone": {
			"<svg>\n  <metadata>m</metadata>\n  <g/>\n</svg>\n",
			"<svg>\n  \n  <g/>\n</svg>\n",
		},
		"comments go everywhere": {
			"<!-- a --><svg><!-- b --><g><!-- c --></g><!-- d --></svg><!-- e -->\n",
			"<svg><g></g></svg>\n",
		},
		"a byte order mark stays": {
			"\xEF\xBB\xBF<?xml version='1.0'?><svg/>",
			"\xEF\xBB\xBF<?xml version='1.0'?><svg/>",
		},
		"instructions go except the stylesheet": {
			`<?xml version="1.0"?><?xml-stylesheet href="a.css"?><?xpacket begin="" id="Test"?><?other x?><svg><?inner y?></svg><?after z?>`,
			`<?xml version="1.0"?><?xml-stylesheet href="a.css"?><svg></svg>`,
		},
		"text and CDATA that look like markup stay": {
			`<svg><text>inkscape:version="1" &lt;metadata&gt;</text><style><![CDATA[<metadata>x</metadata><!-- y -->]]></style></svg>`,
			`<svg><text>inkscape:version="1" &lt;metadata&gt;</text><style><![CDATA[<metadata>x</metadata><!-- y -->]]></style></svg>`,
		},
		"a quoted > does not end a tag": {
			`<svg xmlns:a="urn:a"><g class="a > b" a:x="p > q"/></svg>`,
			`<svg><g class="a > b"/></svg>`,
		},
		"a nested element with the same name": {
			`<svg xmlns:a="urn:a"><a:g><a:g>x</a:g><g>y</g></a:g><g/></svg>`,
			`<svg><g/></svg>`,
		},
		"a CDATA end tag does not end a removed element": {
			`<svg xmlns:a="urn:a"><a:b><![CDATA[</a:b>]]><!-- </a:b> --></a:b><g/></svg>`,
			`<svg><g/></svg>`,
		},
		"trailing content": {
			"<svg/>\n<svg>Test Second Root</svg>trailing",
			"<svg/>\n",
		},
		"trailing garbage after a comment": {
			"<svg/><!-- c -->\ngarbage <<<",
			"<svg/>\n",
		},
		"an unterminated trailing comment": {
			"<svg/> <!-- secret",
			"<svg/> ",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := clean(t, tc.in, core.Options{})
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestPrefixesResolveThroughDeclarations(t *testing.T) {
	t.Run("a renamed prefix is still removed", func(t *testing.T) {
		out, rep := clean(t, `<svg xmlns="http://www.w3.org/2000/svg"
			xmlns:zzz="http://www.inkscape.org/namespaces/inkscape"
			xmlns:yyy="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
			xmlns:qqq="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
			zzz:version="Test Version" yyy:docname="Test Name">
			<yyy:namedview id="Test View"/><qqq:RDF>Test RDF</qqq:RDF><zzz:clipboard>Test Clip</zzz:clipboard><g zzz:label="Test Label"/></svg>`, core.Options{})
		for _, secret := range []string{"Test", "zzz", "yyy", "qqq"} {
			if strings.Contains(out, secret) {
				t.Errorf("%q survived: %s", secret, out)
			}
		}
		for _, field := range []string{"Editor version", "Document name", "Layer or object label"} {
			if !hasField(rep, field, core.ActionRemove) {
				t.Errorf("no finding for %q: %+v", field, rep.Findings)
			}
		}
	})
	t.Run("a familiar prefix on a kept namespace stays", func(t *testing.T) {
		const in = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:inkscape="http://www.w3.org/1999/xlink"><use inkscape:href="#a"/></svg>`
		if out, _ := clean(t, in, core.Options{}); out != in {
			t.Errorf("the file changed: %s", out)
		}
	})
	t.Run("a prefix declared on the element it is used on", func(t *testing.T) {
		out, _ := clean(t, `<svg><g xmlns:a="urn:a" a:x="1" id="k"/><a:b xmlns:a="urn:a">Test</a:b></svg>`, core.Options{})
		if out != `<svg><g id="k"/></svg>` {
			t.Errorf("got %s", out)
		}
	})
	t.Run("rebinding in an inner scope wins and then ends", func(t *testing.T) {
		in := `<svg xmlns:p="http://www.w3.org/1999/xlink"><g xmlns:p="urn:vendor" p:x="Test"><p:y>Test</p:y></g><use p:href="#k"/></svg>`
		out, _ := clean(t, in, core.Options{})
		if want := `<svg xmlns:p="http://www.w3.org/1999/xlink"><g></g><use p:href="#k"/></svg>`; out != want {
			t.Errorf("got %s", out)
		}
	})
	t.Run("a vendor namespace nobody has heard of goes", func(t *testing.T) {
		out, rep := clean(t, `<svg xmlns:v="urn:new:tool:2031" v:owner="Test Owner"><v:data>Test Data</v:data></svg>`, core.Options{})
		if out != `<svg></svg>` {
			t.Errorf("got %s", out)
		}
		if !hasField(rep, "Editor namespace", core.ActionRemove) || !hasField(rep, "Editor element", core.ActionRemove) {
			t.Errorf("findings: %+v", rep.Findings)
		}
	})
	t.Run("a foreign default namespace removes what it covers", func(t *testing.T) {
		out, _ := clean(t, `<svg:svg xmlns:svg="http://www.w3.org/2000/svg" xmlns="urn:vendor"><secret>Test</secret><svg:g/></svg:svg>`, core.Options{})
		if want := `<svg:svg xmlns:svg="http://www.w3.org/2000/svg"><svg:g/></svg:svg>`; out != want {
			t.Errorf("got %s", out)
		}
	})
	t.Run("an entity-valued namespace is resolved without being expanded", func(t *testing.T) {
		in := `<!DOCTYPE svg [<!ENTITY keep "http://www.w3.org/1999/xlink"><!ENTITY drop "urn:vendor"><!-- " ] > --><?x y ?>
			<!ENTITY keep "urn:ignored-second-declaration"><!ENTITY % param "urn:p"><!ELEMENT a (#PCDATA)>]>
			<svg xmlns:k="&keep;" xmlns:d="&drop;" xmlns:u="&undeclared;" d:x="Test" u:y="Test"><use k:href="#a"/><d:z/><u:z/></svg>`
		out, rep := clean(t, in, core.Options{})
		if !strings.Contains(out, `<svg xmlns:k="&keep;"><use k:href="#a"/></svg>`) {
			t.Errorf("got %s", out)
		}
		if !strings.Contains(out, `<!ENTITY drop "urn:vendor">`) {
			t.Errorf("the DOCTYPE was edited: %s", out)
		}
		if len(rep.Warnings) != 1 {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
	t.Run("an undeclared prefix is refused", func(t *testing.T) {
		wantError(t, `<svg><inkscape:thing/></svg>`, core.ErrMalformed)
		wantError(t, `<svg inkscape:version="1"/>`, core.ErrMalformed)
		wantError(t, `<svg><image xlink:href="x"/></svg>`, core.ErrMalformed)
	})
}

func TestRootText(t *testing.T) {
	removed := []string{
		"Created with Sketch.", "created with inkscape", "Generator: Sketch 52.6", "Generated by Test Tool", "Made with Test App",
		"Exported from Test App", "Created by Test Author", "secret-plan.svg", "My Logo FINAL.PNG", "plan.ai",
		`C:\Users\Test Author\plan`, "/home/test/plan", "file:///home/test/plan",
	}
	for _, text := range removed {
		for _, element := range []string{"title", "desc"} {
			t.Run(element+" "+text, func(t *testing.T) {
				in := svgDoc("<" + element + ">" + text + "</" + element + "><rect/>")
				out, rep := clean(t, in, core.Options{})
				if strings.Contains(out, "<"+element) {
					t.Errorf("it survived: %s", out)
				}
				field := map[string]string{"title": "Title", "desc": "Description"}[element]
				if !hasField(rep, field, core.ActionRemove) {
					t.Errorf("findings: %+v", rep.Findings)
				}
			})
		}
	}
	for _, text := range []string{"A map of Paris", "Logo", "Figure 1: the created with tool", "Company.Inc name"} {
		t.Run("kept "+text, func(t *testing.T) {
			in := svgDoc("<title>" + text + "</title><rect/>")
			out, rep := clean(t, in, core.Options{})
			if out != in {
				t.Errorf("got %s", out)
			}
			if !hasField(rep, "Title", core.ActionRetain) {
				t.Errorf("findings: %+v", rep.Findings)
			}
		})
	}
	t.Run("a nested title is judged like any other", func(t *testing.T) {
		in := svgDoc(`<g><title>Logo</title><rect/></g>`)
		out, rep := clean(t, in, core.Options{})
		if out != in || !hasField(rep, "Title", core.ActionRetain) {
			t.Errorf("got %s, findings %+v", out, rep.Findings)
		}
		out, rep = clean(t, svgDoc(`<g><title>Created with Sketch</title><rect/></g>`), core.Options{})
		if strings.Contains(out, "Sketch") || !hasField(rep, "Title", core.ActionRemove) {
			t.Errorf("got %s, findings %+v", out, rep.Findings)
		}
	})
	t.Run("a kept title still gets filtered", func(t *testing.T) {
		in := `<svg xmlns:a="urn:a"><title a:x="Test">Logo<!-- Test --></title></svg>`
		out, _ := clean(t, in, core.Options{})
		if out != `<svg><title>Logo</title></svg>` {
			t.Errorf("got %s", out)
		}
	})
	t.Run("strict removes kept text", func(t *testing.T) {
		out, rep := clean(t, svgDoc(`<title>Logo</title><desc>Text</desc><title/><rect/>`), core.Options{Strict: true})
		if strings.Contains(out, "title") || strings.Contains(out, "desc") {
			t.Errorf("got %s", out)
		}
		if !hasField(rep, "Title", core.ActionRemove) || hasField(rep, "Title", core.ActionRetain) {
			t.Errorf("findings: %+v", rep.Findings)
		}
	})
	t.Run("an empty title has nothing to report", func(t *testing.T) {
		in := svgDoc(`<title></title><title/>`)
		out, rep := clean(t, in, core.Options{})
		if out != in || len(rep.Findings) != 0 {
			t.Errorf("got %s, findings %+v", out, rep.Findings)
		}
	})
	t.Run("entities are decoded before judging", func(t *testing.T) {
		_, rep := clean(t, svgDoc(`<title>Tom &amp; Jerry</title>`), core.Options{})
		if i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Title" }); i < 0 || rep.Findings[i].Value != "Tom & Jerry" {
			t.Errorf("findings: %+v", rep.Findings)
		}
	})
	t.Run("a broken title is refused", func(t *testing.T) {
		wantError(t, `<svg><title>never closed</svg>`, core.ErrMalformed)
		wantError(t, `<svg><title>`, core.ErrMalformed)
	})
}

func TestDoctype(t *testing.T) {
	t.Run("an internal subset stays, with a warning", func(t *testing.T) {
		out, rep := clean(t, illustratorFile, core.Options{})
		if !strings.Contains(out, `<!ENTITY ns_sfw "http://ns.adobe.com/SaveForWeb/1.0/">`) {
			t.Errorf("the subset was edited: %s", out)
		}
		if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, "internal subset") }) {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
	t.Run("entities are not expanded", func(t *testing.T) {
		in := `<!DOCTYPE svg [<!ENTITY who "Test Author">]><svg><text>&who;</text></svg>`
		out, _ := clean(t, in, core.Options{})
		if out != in {
			t.Errorf("got %s", out)
		}
	})
	t.Run("everything a subset may hold is accepted", func(t *testing.T) {
		in := "<!DOCTYPE svg [ %a; <!-- it's ] --> <?p q?>\n<!ELEMENT a (#PCDATA)><!ATTLIST a b CDATA \"]>\"><!NOTATION n SYSTEM 'x'> ] >\n<svg/>"
		if out, _ := clean(t, in, core.Options{}); out != in {
			t.Errorf("got %s", out)
		}
	})
	t.Run("a plain DOCTYPE gets no warning", func(t *testing.T) {
		in := `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd"><svg/>`
		out, rep := clean(t, in, core.Options{})
		if out != in || len(rep.Warnings) != 0 {
			t.Errorf("got %s, warnings %q", out, rep.Warnings)
		}
	})
	t.Run("a local DTD gets a warning", func(t *testing.T) {
		for _, path := range []string{"file:///home/test/svg.dtd", "/home/test/svg.dtd", `C:\Users\Test\svg.dtd`} {
			_, rep := clean(t, `<!DOCTYPE svg SYSTEM "`+path+`"><svg/>`, core.Options{})
			if len(rep.Warnings) != 1 {
				t.Errorf("%s: warnings: %q", path, rep.Warnings)
			}
		}
		_, rep := clean(t, `<!DOCTYPE svg SYSTEM 'file:///a' [<!ENTITY a "b">]><svg/>`, core.Options{})
		if len(rep.Warnings) != 2 {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
	t.Run("a second DOCTYPE and an inner one are refused", func(t *testing.T) {
		wantError(t, `<!DOCTYPE svg><!DOCTYPE svg><svg/>`, core.ErrMalformed)
		wantError(t, `<svg><!DOCTYPE svg></svg>`, core.ErrMalformed)
	})
}

func TestEmbeddedImages(t *testing.T) {
	image := func(attr string, value string) string {
		return svgDoc(`<image ` + attr + `="` + value + `"/>`)
	}
	wrap := func(s string) string {
		var b strings.Builder
		for len(s) > 8 {
			b.WriteString(s[:8] + "&#10;\n \t")
			s = s[8:]
		}
		return b.String() + s
	}
	encoded := b64(secretImage)
	want := func(attr string) string {
		return svgDoc(`<image ` + attr + `="data:image/png;base64,` + b64(cleanImage) + `"/>`)
	}
	for name, tc := range map[string]struct{ attr, value string }{
		"xlink:href":       {"xlink:href", "data:image/png;base64," + encoded},
		"href":             {"href", "data:image/png;base64," + encoded},
		"wrapped":          {"xlink:href", "data:image/png;base64," + wrap(encoded)},
		"upper case":       {"xlink:href", "DATA:image/png;BASE64," + encoded},
		"no padding":       {"xlink:href", "data:image/png;base64," + strings.TrimRight(encoded, "=")},
		"percent-encoded":  {"xlink:href", "data:image/png;base64," + strings.ReplaceAll(encoded, "=", "%3D")},
		"named parameter":  {"xlink:href", "data:image/png;name=secret-photo.png;base64," + encoded},
		"charset and name": {"xlink:href", "data:image/png;charset=utf-8;base64," + encoded},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, image(tc.attr, tc.value), core.Options{})
			if out != want(tc.attr) {
				t.Errorf("got  %s\nwant %s", out, want(tc.attr))
			}
			if !hasField(rep, "Fake secret", core.ActionRemove) || !hasField(rep, "Fake orientation", core.ActionRetain) {
				t.Errorf("findings: %+v", rep.Findings)
			}
		})
	}
	t.Run("feImage counts", func(t *testing.T) {
		out, _ := clean(t, svgDoc(`<filter><feImage xlink:href="data:image/png;base64,`+encoded+`"/></filter>`), core.Options{})
		if strings.Contains(out, encoded) {
			t.Errorf("got %s", out)
		}
	})
	t.Run("an image of a kind no handler knows stays, with a warning", func(t *testing.T) {
		in := image("xlink:href", "data:image/x-emf;name=keep;base64,"+b64("not a known format"))
		out, rep := clean(t, in, core.Options{})
		if out != in {
			t.Errorf("got %s", out)
		}
		if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "cannot clean") {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
	t.Run("a damaged image fails the file", func(t *testing.T) {
		wantError(t, image("href", "data:image/png;base64,"+b64(brokenMagic)), core.ErrMalformed)
	})
	t.Run("invalid base64 fails the file", func(t *testing.T) {
		wantError(t, image("href", "data:image/png;base64,@@@@"), core.ErrMalformed)
		wantError(t, image("href", "data:image/png;base64,%zz"), core.ErrMalformed)
		wantError(t, image("href", "data:image/png;base64"), core.ErrMalformed)
	})
	t.Run("a data URI that is not base64 stays, with a warning", func(t *testing.T) {
		for _, value := range []string{"data:image/svg+xml,%3Csvg/%3E", "data:text/plain;base64," + encoded} {
			in := image("href", value)
			out, rep := clean(t, in, core.Options{})
			if out != in || len(rep.Warnings) != 1 {
				t.Errorf("got %s, warnings %q", out, rep.Warnings)
			}
		}
	})
	t.Run("an image in CSS stays, with a warning", func(t *testing.T) {
		for _, in := range []string{
			svgDoc(`<rect style="fill:url(data:image/png;base64,` + encoded + `)"/>`),
			svgDoc(`<style>.a{background:url(data:image/png;base64,` + encoded + `)}</style>`),
			svgDoc(`<style><![CDATA[.a{background:url(data:image/png;base64,` + encoded + `)}]]></style>`),
			svgDoc(`<use href="data:image/png;base64,` + encoded + `"/>`),
		} {
			out, rep := clean(t, in, core.Options{})
			if out != in || len(rep.Warnings) != 1 {
				t.Errorf("got %s, warnings %q", out, rep.Warnings)
			}
		}
	})
	t.Run("a nested SVG is cleaned", func(t *testing.T) {
		nested := `<svg><metadata>Test Secret</metadata><rect/></svg>`
		out, rep := clean(t, image("href", "data:image/svg+xml;base64,"+b64(nested)), core.Options{})
		if want := image("href", "data:image/svg+xml;base64,"+b64(`<svg><rect/></svg>`)); out != want {
			t.Errorf("got %s", out)
		}
		if !hasField(rep, "Metadata element", core.ActionRemove) {
			t.Errorf("findings: %+v", rep.Findings)
		}
	})
	t.Run("nesting past the limit fails the file", func(t *testing.T) {
		doc := `<svg/>`
		for range core.MaxNestedDepth + 1 {
			doc = image("href", "data:image/svg+xml;base64,"+b64(doc))
		}
		wantError(t, doc, core.ErrTooLarge)
	})
	t.Run("strict reaches the nested image", func(t *testing.T) {
		nested := `<svg><title>Logo</title></svg>`
		out, _ := clean(t, image("href", "data:image/svg+xml;base64,"+b64(nested)), core.Options{Strict: true})
		if want := image("href", "data:image/svg+xml;base64,"+b64(`<svg></svg>`)); out != want {
			t.Errorf("got %s", out)
		}
	})
	t.Run("the decoded size is capped before allocating", func(t *testing.T) {
		if _, err := decodeImage(strings.Repeat("A", maxEmbeddedImage/3*4+8), maxEmbeddedImage); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v", err)
		}
		if _, err := decodeImage(strings.Repeat("A", 64), 16); !errors.Is(err, core.ErrTooLarge) {
			t.Errorf("got %v", err)
		}
		if got, err := decodeImage(strings.Repeat("A", 64), 48); err != nil || len(got) != 48 {
			t.Errorf("got %d bytes, %v", len(got), err)
		}
	})
}

func TestLocalLinks(t *testing.T) {
	for _, link := range []string{
		"file:///home/testauthor/secret.png", "FILE:///C:/Users/Test/secret.png",
		`C:\Users\Test\secret.png`, "C:/Users/Test/secret.png", `\\server\share\secret.png`, "/home/testauthor/secret.png",
	} {
		t.Run(link, func(t *testing.T) {
			for _, in := range []string{
				svgDoc(`<image xlink:href="` + link + `"/>`),
				svgDoc(`<image href="` + link + `"/>`),
				svgDoc(`<use xlink:href="` + link + `"/>`),
				svgDoc(`<g xml:base="` + link + `"/>`),
			} {
				out, rep := clean(t, in, core.Options{})
				if out != in {
					t.Errorf("got %s", out)
				}
				if len(rep.Warnings) != 1 || !hasField(rep, "Linked file path", core.ActionRetain) {
					t.Errorf("warnings %q, findings %+v", rep.Warnings, rep.Findings)
				}
			}
		})
	}
	for _, link := range []string{"#local", "picture.png", "../picture.png", "https://example.com/a.png", "//example.com/a.png"} {
		in := svgDoc(`<image xlink:href="` + link + `"/>`)
		if _, rep := clean(t, in, core.Options{}); len(rep.Warnings) != 0 {
			t.Errorf("%s: warnings %q", link, rep.Warnings)
		}
	}
	t.Run("a stylesheet link", func(t *testing.T) {
		_, rep := clean(t, `<?xml-stylesheet href="file:///home/test/a.css"?><svg/>`, core.Options{})
		if len(rep.Warnings) != 1 {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
	t.Run("warnings do not repeat", func(t *testing.T) {
		_, rep := clean(t, svgDoc(strings.Repeat(`<image href="file:///a"/>`, 100)), core.Options{})
		if len(rep.Warnings) != 1 {
			t.Errorf("warnings: %q", rep.Warnings)
		}
	})
}

func utf16Bytes(s string, order binary.AppendByteOrder, bom bool) []byte {
	var out []byte
	if bom {
		out = order.AppendUint16(out, 0xFEFF)
	}
	for _, unit := range utf16.Encode([]rune(s)) {
		out = order.AppendUint16(out, unit)
	}
	return out
}

func match(data []byte) bool {
	return handler{}.Match(data[:min(len(data), core.HeadSize)], core.BytesSource(data))
}

func TestUTF16IsRefusedLoudly(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-16"?><svg xmlns="http://www.w3.org/2000/svg"><metadata>Test</metadata></svg>`
	for name, data := range map[string][]byte{
		"LE with mark":    utf16Bytes(doc, binary.LittleEndian, true),
		"BE with mark":    utf16Bytes(doc, binary.BigEndian, true),
		"LE without mark": utf16Bytes(doc, binary.LittleEndian, false),
		"BE without mark": utf16Bytes(doc, binary.BigEndian, false),
	} {
		t.Run(name, func(t *testing.T) {
			if !match(data) {
				t.Error("it does not match, so the user would see no specific reason")
			}
			wantError(t, string(data), core.ErrUnsupported)
		})
	}
	if match(utf16Bytes(`<html/>`, binary.LittleEndian, true)) {
		t.Error("UTF-16 HTML matches")
	}
}

func TestEncodings(t *testing.T) {
	for _, encoding := range []string{"ISO-8859-1", "windows-1252", "UTF-16", "Shift_JIS"} {
		wantError(t, `<?xml version="1.0" encoding="`+encoding+`"?><svg/>`, core.ErrUnsupported)
	}
	for _, encoding := range []string{"UTF-8", "utf-8", "US-ASCII"} {
		clean(t, `<?xml version="1.0" encoding="`+encoding+`"?><svg/>`, core.Options{})
	}
	wantError(t, `<?xml version="1.1"?><svg/>`, core.ErrUnsupported)
}

func TestMatch(t *testing.T) {
	for name, in := range map[string]string{
		"bare":            `<svg/>`,
		"attributes":      `<svg width="1">`,
		"declaration":     `<?xml version="1.0"?><svg>`,
		"whitespace":      " \r\n\t<svg>",
		"mark":            "\xEF\xBB\xBF<svg>",
		"comment":         `<!-- <html> --><svg>`,
		"doctype":         `<!DOCTYPE svg PUBLIC "a>b" "c"><svg>`,
		"internal subset": `<!DOCTYPE svg [<!ENTITY a "]>"><!-- ]> -->]><svg>`,
		"all of it":       "\xEF\xBB\xBF<?xml version=\"1.0\"?>\n<!-- a -->\n<!DOCTYPE svg>\n<!-- b -->\n<?other?>\n<svg>",
		"prefixed":        `<svg:svg xmlns:svg="http://www.w3.org/2000/svg">`,
	} {
		if !match([]byte(in)) {
			t.Errorf("%s does not match: %s", name, in)
		}
	}
	for name, in := range map[string]string{
		"empty":                ``,
		"text":                 `hello <svg>`,
		"html":                 `<html><svg/></html>`,
		"longer name":          `<svgfoo/>`,
		"upper case":           `<SVG/>`,
		"gzip":                 "\x1F\x8B\x08\x00<svg>",
		"cut comment":          `<!-- <svg>`,
		"cut doctype":          `<!DOCTYPE svg <svg>`,
		"end tag":              `</svg>`,
		"bang":                 `<!ELEMENT a><svg>`,
		"cut instruction":      `<?xml <svg>`,
		"nothing after prolog": `<?xml version="1.0"?>`,
	} {
		if match([]byte(in)) {
			t.Errorf("%s matches: %s", name, in)
		}
	}
	t.Run("a root past the head", func(t *testing.T) {
		long := "<!--" + strings.Repeat("x", core.HeadSize) + "--><svg/>"
		if !match([]byte(long)) {
			t.Error("does not match")
		}
		out, _ := clean(t, long, core.Options{})
		if out != "<svg/>" {
			t.Errorf("got %q", out)
		}
	})
	t.Run("a root past the prolog limit", func(t *testing.T) {
		long := "<!--" + strings.Repeat("x", maxProlog) + "--><svg/>"
		if match([]byte(long)) {
			t.Error("it matches")
		}
		wantError(t, long, core.ErrUnsupported)
	})
	t.Run("a root in another namespace matches but is refused", func(t *testing.T) {
		in := `<svg xmlns="urn:not-svg"/>`
		if !match([]byte(in)) {
			t.Error("does not match")
		}
		wantError(t, in, core.ErrUnsupported)
		wantError(t, `<a:svg xmlns:a="urn:not-svg"/>`, core.ErrUnsupported)
	})
}

func TestMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"never closed":                `<svg><g>`,
		"wrong end tag":               `<svg><g></h></svg>`,
		"crossed tags":                `<svg><a><b></a></b></svg>`,
		"unquoted attribute":          `<svg a=1/>`,
		"attribute without value":     `<svg a/>`,
		"duplicate attribute":         `<svg a="1" a="2"/>`,
		"no space between":            `<svg a="1"b="2"/>`,
		"< in a value":                `<svg a="<"/>`,
		"unterminated value":          `<svg a="1/>`,
		"unterminated tag":            `<svg a="1"`,
		"cut after name":              `<svg`,
		"bad reference in text":       `<svg>a & b</svg>`,
		"bad reference in value":      `<svg a="&"/>`,
		"bad char reference":          `<svg>&#1;</svg>`,
		"huge char reference":         `<svg>&#xFFFFFFFFF;</svg>`,
		"empty reference":             `<svg>&;</svg>`,
		"]]> in text":                 `<svg>]]></svg>`,
		"unterminated comment":        `<svg><!-- </svg>`,
		"unterminated CDATA":          `<svg><![CDATA[ </svg>`,
		"unterminated instruction":    `<svg><?a </svg>`,
		"bad instruction target":      `<svg><?a/b?></svg>`,
		"other markup":                `<svg><!ELEMENT a></svg>`,
		"two colons":                  `<a:b:svg/>`,
		"leading colon":               `<svg :a="1"/>`,
		"trailing colon":              `<svg a:="1"/>`,
		"NUL":                         "<svg>\x00</svg>",
		"control character":           "<svg>\x01</svg>",
		"invalid UTF-8":               "<svg>\xFF</svg>",
		"U+FFFE":                      "<svg>\xEF\xBF\xBE</svg>",
		"late declaration":            `<!-- a --><?xml version="1.0"?><svg/>`,
		"inner declaration":           `<svg><?xml version="1.0"?></svg>`,
		"extra declaration field":     `<?xml version="1.0" author="Test Author"?><svg/>`,
		"bad declaration":             `<?xml version?><svg/>`,
		"empty prefix binding":        `<svg xmlns:a=""/>`,
		"element named xmlns":         `<svg><xmlns:a/></svg>`,
		"text before the root":        `<?xml version="1.0"?>text<svg/>`,
		"CDATA before the root":       `<![CDATA[x]]><svg/>`,
		"removed subtree cut":         `<svg><metadata><a></metadata></svg>`,
		"removed subtree open":        `<svg><metadata>`,
		"DOCTYPE in removed":          `<svg><metadata><!DOCTYPE a></metadata></svg>`,
		"reference with a space":      `<svg>&a b;</svg>`,
		"self-closing with space":     `<svg / >`,
		"end tag with attribute":      `<svg></svg a="1">`,
		"cut end tag":                 `<svg></svg`,
		"quote in a nameless DOCTYPE": `<!DOCTYPE["0]><svg></svg>">`,
		"DOCTYPE with no name":        `<!DOCTYPE><svg/>`,
		"junk in a DOCTYPE":           `<!DOCTYPE svg junk><svg/>`,
		"stray quote in a subset":     `<!DOCTYPE svg ["]><svg/>`,
		"text after a subset":         `<!DOCTYPE svg [] x><svg/>`,
		"second subset":               `<!DOCTYPE svg [][]><svg/>`,
		"quote in a DOCTYPE PI":       `<!DOCTYPE svg [<?x " ?>]><svg/>`,
		"< in a declaration":          `<!DOCTYPE svg [<!ELEMENT a <b>>]><svg/>`,
		"> alone in a subset":         `<!DOCTYPE svg [>]><svg/>`,
		"bad parameter entity":        `<!DOCTYPE svg [%a]><svg/>`,
		"unterminated declaration":    `<!DOCTYPE svg [<!ENTITY a "b"`,
		"unterminated string":         `<!DOCTYPE svg SYSTEM "a><svg/>`,
		"unterminated subset comment": `<!DOCTYPE svg [<!-- ]><svg/>`,
		"unterminated subset PI":      `<!DOCTYPE svg [<?a ]><svg/>`,
		"unterminated subset":         `<!DOCTYPE svg [ <svg/>`,
	} {
		t.Run(name, func(t *testing.T) {
			// Damage in the prolog stops the file from looking like SVG at all, so
			// either refusal is right. Accepting it is not.
			_, err := handler{}.Inspect(context.Background(), core.BytesSource([]byte(in)), core.Options{})
			if !errors.Is(err, core.ErrMalformed) && !errors.Is(err, core.ErrUnsupported) {
				t.Errorf("got %v", err)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	t.Run("deep nesting", func(t *testing.T) {
		wantError(t, "<svg>"+strings.Repeat("<g>", maxDepth+1), core.ErrTooLarge)
		wantError(t, "<svg><metadata>"+strings.Repeat("<g>", maxDepth+1), core.ErrTooLarge)
	})
	t.Run("too many declarations", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("<svg")
		for i := range maxBindings + 1 {
			fmt.Fprintf(&b, ` xmlns:p%d="urn:%d"`, i, i)
		}
		b.WriteString("/>")
		wantError(t, b.String(), core.ErrTooLarge)
	})
	t.Run("the report stops growing", func(t *testing.T) {
		_, rep := clean(t, svgDoc(strings.Repeat("<!-- c -->", maxFindings*3)), core.Options{})
		if len(rep.Findings) != maxFindings+1 {
			t.Errorf("%d findings", len(rep.Findings))
		}
		_, rep = clean(t, svgDoc(strings.Repeat("<metadata><rdf:RDF xmlns:rdf='urn:x'/></metadata>", maxFindings*3)), core.Options{})
		if len(rep.Findings) != maxFindings+1 {
			t.Errorf("%d findings", len(rep.Findings))
		}
		_, rep = clean(t, svgDoc(strings.Repeat(`<g xlink:href="file:///a"/>`, maxFindings*3)), core.Options{})
		if len(rep.Findings) != maxFindings {
			t.Errorf("%d findings", len(rep.Findings))
		}
		_, rep = clean(t, svgDoc(strings.Repeat(`<image href="data:image/png;base64,`+b64(secretImage)+`"/>`, maxFindings*3)), core.Options{})
		if !hasField(rep, "Further metadata", core.ActionRemove) || len(rep.Findings) > maxFindings+2 {
			t.Errorf("%d findings", len(rep.Findings))
		}
	})
	t.Run("a cancelled context stops the walk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := handler{}.Inspect(ctx, core.BytesSource([]byte(svgDoc(`<g/>`))), core.Options{})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	})
}

func TestDecodedMetadata(t *testing.T) {
	rep, err := handler{}.Inspect(context.Background(), core.BytesSource([]byte(inkscapeFile)), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{
		"Creator":          "Test Author",
		"Rights":           "Test Rights Holder",
		"Document name":    "secret-plan.svg",
		"Document folder":  "/home/testauthor/Documents",
		"Export file name": `C:\Users\Test Author\Documents\secret-plan.png`,
		"XML comment":      "Created with Inkscape (http://www.inkscape.org/)",
	} {
		i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == field })
		if i < 0 {
			t.Errorf("no finding for %q", field)
		} else if rep.Findings[i].Value != value {
			t.Errorf("%s = %q, want %q", field, rep.Findings[i].Value, value)
		}
	}
	t.Run("a renamed RDF prefix still decodes", func(t *testing.T) {
		in := `<svg xmlns:r="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:who="http://purl.org/dc/elements/1.1/">
			<metadata><r:RDF><r:Description><who:creator>Test Author</who:creator></r:Description></r:RDF></metadata></svg>`
		rep, err := handler{}.Inspect(context.Background(), core.BytesSource([]byte(in)), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(rep.Findings, func(f core.Finding) bool { return f.Field == "Creator" }); i < 0 || rep.Findings[i].Value != "Test Author" {
			t.Errorf("findings: %+v", rep.Findings)
		}
	})
	t.Run("RDF outside a metadata element decodes too", func(t *testing.T) {
		in := `<svg xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/">
			<rdf:RDF><dc:creator>Test Author</dc:creator></rdf:RDF></svg>`
		out, rep := clean(t, in, core.Options{})
		if strings.Contains(out, "Test Author") || !hasField(rep, "Creator", core.ActionRemove) {
			t.Errorf("got %s, findings %+v", out, rep.Findings)
		}
	})
}

func TestOutputIsWellFormed(t *testing.T) {
	for name, in := range map[string]string{"Inkscape": inkscapeFile, "Illustrator": illustratorFile, "Sketch": sketchFile} {
		t.Run(name, func(t *testing.T) {
			out, _ := clean(t, in, core.Options{})
			dec := newDecoder(out)
			for {
				_, err := dec.Token()
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					t.Fatalf("encoding/xml rejects the output: %v\n%s", err, out)
				}
			}
		})
	}
}

func TestFormat(t *testing.T) {
	f := handler{}.Format()
	if f.ID != "svg" || !f.ResidueWarnOnly || !slices.Contains(f.Extensions, ".svg") || len(f.Limitations) == 0 {
		t.Errorf("Format is %+v", f)
	}
}

func TestRobustness(t *testing.T) {
	for name, in := range map[string]string{"Inkscape": inkscapeFile, "Illustrator": illustratorFile, "Sketch": sketchFile} {
		t.Run(name, func(t *testing.T) { coretest.Robustness(t, handler{}, []byte(in)) })
	}
}

// wellFormed reports whether encoding/xml accepts the whole of data.
func wellFormed(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Entity = xml.HTMLEntity
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func FuzzClean(f *testing.F) {
	f.Add([]byte(inkscapeFile))
	f.Add([]byte(illustratorFile))
	f.Add([]byte(sketchFile))
	f.Add([]byte(svgDoc(`<image xlink:href="data:image/png;base64,` + b64(secretImage) + `"/><title>Logo</title>`)))
	f.Add([]byte(`<!DOCTYPE svg [<!ENTITY a "urn:b">]><svg xmlns:c="&a;" c:d="e"><c:f/><!-- g --><?h i?></svg>trailing`))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
		// Cleaning must not turn a document encoding/xml accepts into one it rejects.
		var out bytes.Buffer
		if _, err := (handler{}).Clean(context.Background(), core.BytesSource(data), &out, core.Options{}); err == nil && wellFormed(data) && !wellFormed(out.Bytes()) {
			t.Errorf("the output is not well-formed:\n input: %q\noutput: %q", data, out.Bytes())
		}
	})
}
