package matroska

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

// marker stands for any text from a file. Field and Where are printed even by clean,
// which promises to print no metadata, so they must never hold any of it. Value is
// hidden there. Errors may quote it, since the command masks what is in quotes.
const marker = "ZZMARKER"

func hasMarkerInValue(rep *core.Report) bool {
	return slices.ContainsFunc(rep.Findings, func(f core.Finding) bool { return strings.Contains(f.Value, marker) })
}

func TestFileTextStaysOutOfPrintedFields(t *testing.T) {
	tag := func(body ...[]byte) []byte {
		return el(idTags, el(idTag, el(idSimpleTag, slices.Concat(body...))))
	}
	fileWith := func(extra ...[]byte) []byte {
		return slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo(), el(idTracks, videoTrack()), slices.Concat(extra...)))
	}
	comment := slices.Concat([]byte("\x03vorbis"), vorbisTags("Test Vendor", marker+"=x"), []byte{1})
	flac := slices.Concat([]byte("fLaC"), flacBlockBytes(flacVorbisComment, true, vorbisTags("v", marker+"=x")))
	for name, data := range map[string][]byte{
		"custom tag":         fileWith(tag(text(idTagName, marker), text(idTagString, "x"))),
		"tag value":          fileWith(tag(text(idTagName, "ARTIST"), text(idTagString, marker))),
		"nested custom tag":  fileWith(tag(text(idTagName, "ARTIST"), el(idSimpleTag, text(idTagName, marker), text(idTagString, "x")))),
		"Vorbis comment":     fileWithTrack(audioTrack(codecVorbis, vorbisPrivate(comment))),
		"FLAC comment":       fileWithTrack(audioTrack(codecFLAC, flac)),
		"attachment":         fileWith(el(idAttachments, el(idAttachedFile, text(idFileName, marker), text(idFileMediaType, marker), text(idFileDescription, marker)))),
		"chapter":            fileWith(el(idChapters, el(idEditionEntry, el(idChapterAtom, el(idChapterDisplay, text(idChapString, marker)))))),
		"title":              fileWith(el(idInfo, text(0x7BA9, marker))),
		"subtitle field":     fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("[Script Info]\n"+marker+": x\n"))),
		"subtitle value":     fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("[Script Info]\nOriginal Script: "+marker+"\n"))),
		"subtitle section":   fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("["+marker+"]\nAudio File: x\n"))),
		"subtitle free text": fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("[Script Info]\n"+marker+"\n"))),
		"subtitle comment":   fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("[Script Info]\n; "+marker+"\n"))),
		"language":           fileWithElementInTrack(text(idLanguage, marker+" x")),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data)
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
			// Inspect is there to show it, so it must not simply vanish.
			if !hasMarkerInValue(rep) {
				t.Errorf("no value holds the planted name: %+v", rep.Findings)
			}
		})
	}
}

func TestKnownTagsKeepTheirNames(t *testing.T) {
	for _, name := range []string{"ARTIST", "artist", "Artist", "ENCODER", "DURATION", "RECORDING_LOCATION", "com.apple.quicktime.location.ISO6709"} {
		data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTags, el(idTag, el(idSimpleTag, text(idTagName, name), text(idTagString, "x"))))))
		_, rep := clean(t, data)
		if len(rep.Findings) != 1 {
			t.Fatalf("%s: findings %+v", name, rep.Findings)
		}
		if f := rep.Findings[0]; f.Field != "Tag "+strings.ToUpper(name) || f.Value != "x" {
			t.Errorf("%s: finding %+v", name, f)
		}
	}
	// A custom name still gets its risk from what the name says.
	data := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTags, el(idTag, el(idSimpleTag, text(idTagName, "MY_GPS_FIX"), text(idTagString, "x"))))))
	_, rep := clean(t, data)
	if f := rep.Findings[0]; f.Field != "Custom tag" || f.Value != "MY_GPS_FIX=x" || f.Category != core.CatLocation || f.Risk != core.RiskHigh {
		t.Errorf("finding %+v", f)
	}
}

var quotedInError = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func TestFileTextInErrorsIsQuoted(t *testing.T) {
	_, err := analyse(t.Context(), core.BytesSource(ebmlHeader(marker)))
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(quotedInError.ReplaceAllString(err.Error(), ""), marker) {
		t.Errorf("unquoted file text in %q", err)
	}
}
