package xmp

import (
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
)

func FuzzFindings(f *testing.F) {
	f.Add(coretest.FakeXMP())
	for _, encoded := range encodings(string(coretest.FakeXMP())) {
		f.Add(encoded)
	}
	f.Add([]byte(description(`exif:GPSLatitude="48,51.5N" exif:GPSLongitude="2,17.67W"`, "")))
	f.Add([]byte(description("", `<xmpMM:History><rdf:Seq><rdf:li/></rdf:Seq></xmpMM:History><dc:title><rdf:Alt><rdf:li xml:lang="fr">x</rdf:li></rdf:Alt></dc:title>`)))
	f.Add([]byte(`<rdf:Description dc:creator="cut off`))
	f.Add([]byte(`<dc:creator><![CDATA[cut off`))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY e "e">]><dc:creator>&e;</dc:creator>`))
	f.Add([]byte("<\x00a\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, packet []byte) {
		checkPacket(t, packet)
		Contains(packet)

		// A well-formed packet never reaches the scan through Findings, so it is
		// driven directly here or those paths would go unfuzzed for most inputs.
		text := decodeText(packet)
		scanned := newWalker()
		scan(text, scanned)
		checkFindings(t, scanned.findings("W"))

		parsed := newWalker()
		if parseXML(text, parsed) == nil {
			checkFindings(t, parsed.findings("W"))
		}
	})
}
