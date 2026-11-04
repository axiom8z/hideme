package avi

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// marker stands for any text from a file: four bytes, where the format's identifiers
// are that long. Field and Where are printed even by clean, which promises to print no
// metadata, so they must never hold any of it. Errors may quote it, since the command
// masks what is in quotes.
const (
	marker     = "ZZMK"
	longMarker = "ZZMARKER"
)

func TestFileTextStaysOutOfPrintedFields(t *testing.T) {
	header := func(extra ...[]byte) []byte {
		return list("hdrl", chunk("avih", []byte("HH")), streamList(), slices.Concat(extra...))
	}
	for name, data := range map[string][]byte{
		"INFO entry ID":      riffChunk(formAVI, header(), list("INFO", chunk(marker, []byte("x"))), movi()),
		"INFO entry value":   riffChunk(formAVI, header(), list("INFO", chunk("IART", []byte(longMarker))), movi()),
		"INFO in a header":   riffChunk(formAVI, header(list("INFO", chunk(marker, []byte(longMarker)))), movi()),
		"chunk at the top":   riffChunk(formAVI, header(), chunk(marker, []byte(longMarker)), movi()),
		"list at the top":    riffChunk(formAVI, header(), list(marker, chunk(marker, []byte(longMarker))), movi()),
		"chunk in hdrl":      riffChunk(formAVI, header(chunk(marker, []byte(longMarker))), movi()),
		"chunk in strl":      riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList(chunk(marker, []byte(longMarker)))), movi()),
		"chunk in odml":      riffChunk(formAVI, header(list("odml", chunk(marker, []byte(longMarker)))), movi()),
		"chunk in movi":      aviFile(frame(), chunk(marker, []byte(longMarker))),
		"list in movi":       aviFile(frame(), list(marker, chunk(marker, []byte(longMarker)))),
		"unusual chunk type": aviFile(frame(), chunk("00ZZ", []byte(longMarker))),
		"stream name":        riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList(chunk("strn", []byte(longMarker)))), movi()),
		"text stream":        riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), list("strl", chunk("strh", []byte(marker+"xx")))), list("movi")),
		"strf text":          riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), list("strl", chunk("strh", []byte("vids")), chunk("strf", []byte(longMarker+longMarker)))), movi()),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data)
			for _, f := range rep.Findings {
				for _, s := range []string{f.Field, f.Where, f.Reason} {
					if strings.Contains(s, marker) || strings.Contains(s, longMarker) || strings.Contains(s, "00ZZ") {
						t.Errorf("file text reached a printed part of a finding: %+v", f)
					}
				}
			}
			for _, w := range rep.Warnings {
				if strings.Contains(w, marker) || strings.Contains(w, longMarker) {
					t.Errorf("file text reached a warning: %q", w)
				}
			}
		})
	}
}

var quotedInError = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func TestFileTextInErrorsIsQuoted(t *testing.T) {
	for name, data := range map[string][]byte{
		"other form":       riffChunk(marker),
		"frame outside":    riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), list("movi"), chunk("00dc", []byte("x"))),
		"oversized header": riffChunk(formAVI, list("hdrl", chunk("avih", make([]byte, maxHeaderChunk+2))), movi()),
		"overrun":          riffChunk(formAVI, []byte(marker+"\xFF\x00\x00\x00")),
		"tiny list":        riffChunk(formAVI, chunk("LIST", []byte("ab")), chunk(marker)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := analyse(t.Context(), core.BytesSource(data))
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(quotedInError.ReplaceAllString(err.Error(), ""), marker) {
				t.Errorf("unquoted file text in %q", err)
			}
		})
	}
}
