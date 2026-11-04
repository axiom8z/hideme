package matroska

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

const (
	idBlockGroup      = 0xA0
	idBlock           = 0xA1
	idBlockAdditions  = 0x75A1
	idBlockMore       = 0xA6
	idBlockAddID      = 0xEE
	idBlockAdditional = 0xA5
	idCodecState      = 0xA4
	idEncryptedBlock  = 0xAF
	idSilentTracks    = 0x5854
	idPrevSize        = 0xAB
	idLanguage        = 0x22B59C
	idLanguageBCP47   = 0x22B59D
	idChannelPos      = 0x7D7B
	idAudio           = 0xE1

	secretText = "Test Author, 1 Secret Road"
)

// simpleBlock is a block of track 1, the track the fixtures declare.
func simpleBlock() []byte { return el(idSimpleBlock, blockPayload(1)) }

func blockPayload(track byte) []byte {
	return slices.Concat([]byte{0x80 | track, 0, 0, 0x80}, []byte(mediaBytes))
}

// clusterFile puts children in a known-size cluster of a file with one track.
func clusterFile(children ...[]byte) []byte {
	return slices.Concat(ebmlHeader(docMatroska), el(idSegment,
		plainInfo(), el(idTracks, videoTrack()),
		el(idCluster, num(idTimestamp, 0), slices.Concat(children...))))
}

func TestClusterContentsAreHeldToAnAllowlist(t *testing.T) {
	block := simpleBlock()
	for name, hidden := range map[string][]byte{
		"unknown element":           el(0x4D7E, []byte(secretText)),
		"Void with old contents":    el(idVoid, []byte(secretText)),
		"unknown BlockGroup child":  el(idBlockGroup, el(idBlock, blockPayload(1)), el(0x6543, []byte(secretText))),
		"Void in a BlockGroup":      el(idBlockGroup, el(idBlock, blockPayload(1)), el(idVoid, []byte(secretText))),
		"unknown SilentTracks kid":  el(idSilentTracks, el(0x4D7E, []byte(secretText))),
		"block of no track":         el(idSimpleBlock, slices.Concat([]byte{0x8F, 0, 0, 0x80}, []byte(secretText))),
		"grouped block of no track": el(idBlockGroup, el(idBlock, slices.Concat([]byte{0x8F, 0, 0, 0}, []byte(secretText)))),
	} {
		t.Run(name, func(t *testing.T) {
			data := clusterFile(block, hidden, block)
			out, rep := clean(t, data)
			if bytes.Contains(out, []byte(secretText)) {
				t.Error("the planted text survived")
			}
			if len(rep.Residual()) == 0 {
				t.Error("nothing was reported")
			}
			if len(out) != len(data) {
				t.Errorf("length changed from %d to %d", len(data), len(out))
			}
			if n := bytes.Count(out, []byte(mediaBytes)); n < 2 {
				t.Errorf("%d of the real blocks survived", n)
			}
			again, _ := clean(t, out)
			if !bytes.Equal(again, out) {
				t.Error("a second clean changed the file")
			}
		})
	}
}

func TestClusterRefusals(t *testing.T) {
	block := simpleBlock()
	wantError(t, clusterFile(block, el(idPrevSize, []byte(secretText))), core.ErrMalformed)
	wantError(t, clusterFile(block, el(idEncryptedBlock, []byte(secretText))), core.ErrEncrypted)
	// A top-level element in a known-size cluster means the sizes cannot be trusted:
	// readers disagree on whether it ends the cluster, and what follows may be video.
	for _, id := range []uint32{idCluster, idInfo, idTracks, idCues, idTags, idSeekHead, idAttachments, idChapters, idSegment, idEBML} {
		wantError(t, clusterFile(block, el(id, []byte(secretText))), core.ErrMalformed)
	}
	// A child that overruns its cluster is damage, as is a stray byte.
	broken := clusterFile(block)
	overrun := slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTracks, videoTrack()),
		el(idCluster, []byte{0xA3, 0x90, 1, 2}), el(idVoid, make([]byte, 40))))
	wantError(t, overrun, core.ErrMalformed)
	if out, _ := clean(t, broken); !bytes.Equal(out, broken) {
		t.Error("a sound cluster changed")
	}
	// Blocks with no track table at all can only be a file this parser does not
	// understand, and voiding them would destroy it.
	noTracks := slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo(), el(idCluster, num(idTimestamp, 0), block)))
	wantError(t, noTracks, core.ErrMalformed)
}

func TestClusterKeepsWhatDecodersNeed(t *testing.T) {
	data := clusterFile(
		el(idSimpleBlock, blockPayload(1)),
		el(idBlockGroup, el(idBlock, blockPayload(1)), num(0x9B, 40), num(0xFB, 1), num(0xFA, 1), num(0x75A2, 2)),
		num(idPrevSize, 7), num(0xA7, 9), el(idSilentTracks, num(0x58D7, 2)),
		el(idVoid, make([]byte, 16)))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("a sound cluster changed")
	}
	if len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
		t.Errorf("a sound cluster gave findings %+v, warnings %q", rep.Findings, rep.Warnings)
	}
}

// A multi-byte track number must be read as one number, and tracks may come after
// the clusters that use them.
func TestBlockTrackNumbers(t *testing.T) {
	wide := el(idTrackEntry, num(idTrackNumber, 300), num(idTrackType, trackVideo), text(idCodecID, "V_VP9"))
	block := el(idSimpleBlock, slices.Concat([]byte{0x41, 0x2C, 0, 0, 0x80}, []byte(mediaBytes)))
	data := slices.Concat(ebmlHeader(docMatroska), el(idSegment,
		plainInfo(), el(idCluster, num(idTimestamp, 0), block), el(idTracks, wide)))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) || len(rep.Findings) != 0 {
		t.Errorf("a block of a wide track number changed: %+v", rep.Findings)
	}
}

func TestClusterCRCGoesWhenItsContentsChange(t *testing.T) {
	crc := el(idCRC32, binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(simpleBlock())))
	data := clusterFile(crc, simpleBlock(), el(0x4D7E, []byte(secretText)))
	out, _ := clean(t, data)
	if bytes.Contains(out, crc) {
		t.Error("a stale CRC-32 survived")
	}
	sound := clusterFile(crc, simpleBlock())
	if out, _ := clean(t, sound); !bytes.Contains(out, crc) {
		t.Error("the CRC-32 of an untouched cluster went")
	}
}

func TestBlockAdditionsWarn(t *testing.T) {
	data := clusterFile(el(idBlockGroup, el(idBlock, blockPayload(1)),
		el(idBlockAdditions, el(idBlockMore, num(idBlockAddID, 1), el(idBlockAdditional, []byte("alpha"))))))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("block additions changed")
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "block additions") {
		t.Errorf("warnings = %q", rep.Warnings)
	}
	wantError(t, clusterFile(el(idBlockGroup, el(idBlock, blockPayload(1)),
		el(idBlockAdditions, el(idBlockMore, unknownEl(idBlockAdditional))))), core.ErrMalformed)
}

func TestCodecStateIsChecked(t *testing.T) {
	data := clusterFile(el(idBlockGroup, el(idBlock, blockPayload(1)), el(idCodecState, []byte(secretText))))
	if _, rep := clean(t, data); len(rep.Warnings) != 1 {
		t.Errorf("warnings = %q", rep.Warnings)
	}
}

func TestTrackLanguageIsNotFreeText(t *testing.T) {
	for _, id := range []uint32{idLanguage, idLanguageBCP47} {
		data := fileWithElementInTrack(text(id, secretText))
		out, rep := clean(t, data)
		if bytes.Contains(out, []byte(secretText)) {
			t.Errorf("a language of %#x kept free text", id)
		}
		if len(rep.Residual()) == 0 {
			t.Error("nothing was reported")
		}
		// Hyphens alone do not make a language.
		data = fileWithElementInTrack(text(id, "Testauthor-Secret-Road"))
		if out, _ := clean(t, data); bytes.Contains(out, []byte("Testauthor")) {
			t.Errorf("a language of %#x kept a hyphenated name", id)
		}
		// NULs would hide the rest from a reader, not from a search of the bytes.
		data = fileWithElementInTrack(el(id, []byte("eng\x00"+secretText)))
		if out, _ := clean(t, data); bytes.Contains(out, []byte(secretText)) {
			t.Errorf("a language of %#x kept text after a NUL", id)
		}
	}
	for _, lang := range []string{"und", "eng", "en", "en-US", "zh-Hans", "sr-Cyrl-RS", "de-1996", "eng\x00\x00"} {
		data := fileWithElementInTrack(text(idLanguage, lang))
		if out, rep := clean(t, data); !bytes.Equal(out, data) || len(rep.Findings) != 0 {
			t.Errorf("language %q changed: %+v", lang, rep.Findings)
		}
	}
}

func fileWithElementInTrack(extra []byte) []byte {
	return slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo(), el(idTracks, videoTrack(extra)), cluster()))
}

func TestCodecIDIsNotFreeText(t *testing.T) {
	file := func(codec string) []byte {
		kind := uint64(trackSubtitle)
		switch codec[0] {
		case 'V':
			kind = trackVideo
		case 'A':
			kind = trackAudio
		}
		return fileWithTrack(el(idTrackEntry, num(idTrackNumber, 1), num(idTrackType, kind), text(idCodecID, codec)))
	}
	for _, codec := range []string{"V_VP9 " + secretText, "V_VP9\x00" + secretText, "V_VP9\n"} {
		wantError(t, file(codec), core.ErrMalformed)
	}
	for _, codec := range []string{"V_VP9", "V_MPEG4/ISO/AVC", "A_AAC/MPEG4/LC/SBR", "V_MS/VFW/FOURCC", "S_TEXT/UTF8", "A_REAL/28_8", "V_MPEGH/ISO/HEVC", "D_WEBVTT/SUBTITLES"} {
		data := file(codec)
		if out, _ := clean(t, data); !bytes.Equal(out, data) {
			t.Errorf("codec %q changed", codec)
		}
	}
}

// ffmpeg matches a codec ID by its prefix, so a suffix must not get a comment header
// past the check that blanks it.
func TestCodecMatchesLikeAReader(t *testing.T) {
	for _, codec := range []string{codecVorbis + "X", codecVorbis + "/1"} {
		data := fileWithTrack(audioTrack(codec, vorbisPrivate(taggedVorbisComment())))
		out, _ := clean(t, data)
		if bytes.Contains(out, []byte("Test Author")) {
			t.Errorf("codec %q kept the comment header", codec)
		}
	}
}

func TestChannelPositionsGo(t *testing.T) {
	data := fileWithElementInTrack(el(idAudio, el(idChannelPos, []byte(secretText))))
	if out, _ := clean(t, data); bytes.Contains(out, []byte(secretText)) {
		t.Error("ChannelPositions kept text that nothing reads")
	}
}

func TestReadableTextInOpaqueDataWarns(t *testing.T) {
	bin := []byte{1, 0x4D, 0x40, 0x1F, 0xFF, 0xE1, 0, 0x17, 0x67, 0x4D, 0x40, 0x1F, 0x96, 0x52}
	for name, data := range map[string][]byte{
		"codec private": fileWithElementInTrack(el(idCodecPrivate, slices.Concat(bin, []byte(secretText)))),
		"UTF-16":        fileWithElementInTrack(el(idCodecPrivate, slices.Concat(bin, utf16LE(secretText)))),
		"Vorbis setup": fileWithTrack(audioTrack(codecVorbis, slices.Concat(
			vorbisPrivate(taggedVorbisComment()), []byte(secretText)))),
		"projection": fileWithElementInTrack(el(idVideo, el(0x7670, el(0x7672, []byte(secretText))))),
		"compression settings": fileWithElementInTrack(el(idContentEncodings, el(0x6240, el(0x5034,
			num(0x4254, 3), el(0x4255, []byte(secretText)))))),
		"block addition data": fileWithElementInTrack(el(0x41E4, el(0x41ED, []byte(secretText)))),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data)
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "readable text") {
				t.Errorf("warnings = %q", rep.Warnings)
			}
		})
	}
	t.Run("binary", func(t *testing.T) {
		data := fileWithElementInTrack(el(idCodecPrivate, bin))
		if _, rep := clean(t, data); len(rep.Warnings) != 0 {
			t.Errorf("binary codec data warned: %q", rep.Warnings)
		}
	})
	t.Run("a long run of one value", func(t *testing.T) {
		// Real Vorbis setup headers hold dozens of U in a row.
		data := fileWithElementInTrack(el(idCodecPrivate, slices.Concat(bin, bytes.Repeat([]byte("U"), 64))))
		if _, rep := clean(t, data); len(rep.Warnings) != 0 {
			t.Errorf("repeated bytes warned: %q", rep.Warnings)
		}
	})
	t.Run("too large to read", func(t *testing.T) {
		data := fileWithElementInTrack(el(idCodecPrivate, make([]byte, maxCodecHeaderRead+1)))
		if _, rep := clean(t, data); len(rep.Warnings) != 1 {
			t.Errorf("warnings = %q", rep.Warnings)
		}
	})
}

func utf16LE(s string) []byte {
	var out []byte
	for _, c := range []byte(s) {
		out = append(out, c, 0)
	}
	return out
}

// assHeader follows what Aegisub writes, with its project section included, and the
// fields that name people filled in with fake values.
const assHeader = "[Script Info]\r\n" +
	"; Script generated by Aegisub 3.2.2\r\n" +
	"; http://www.aegisub.org/\r\n" +
	"Title: Test Episode Title\r\n" +
	"Original Script: Test Author\r\n" +
	"Original Translation: Test Translator\r\n" +
	"Original Editing: Test Editor\r\n" +
	"Original Timing: Test Timer\r\n" +
	"Script Updated By: Test Updater\r\n" +
	"Update Details: Test Details\r\n" +
	"ScriptType: v4.00+\r\n" +
	"WrapStyle: 0\r\n" +
	"ScaledBorderAndShadow: yes\r\n" +
	"YCbCr Matrix: TV.709\r\n" +
	"PlayResX: 1920\r\n" +
	"PlayResY: 1080\r\n" +
	"Video File: C:/Users/Test Person/video.mkv\r\n" +
	"\r\n" +
	"[Aegisub Project Garbage]\r\n" +
	"Audio File: C:/Users/Test Person/audio.wav\r\n" +
	"Last Style Storage: Test Storage\r\n" +
	"\r\n" +
	"[V4+ Styles]\r\n" +
	"; a comment by Test Commenter\r\n" +
	"Format: Name, Fontname, Fontsize, PrimaryColour\r\n" +
	"Style: Default,Arial,20,&H00FFFFFF\r\n" +
	"\r\n" +
	"[Fonts]\r\n" +
	"fontname: test.ttf\r\n" +
	"TestEncodedFontData\r\n" +
	"\r\n" +
	"[Events]\r\n" +
	"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\r\n" +
	"Comment: 0,0:00:00.00,0:00:01.00,Default,Test Commenter,0,0,0,,Test note\r\n"

func subtitleTrack(codec string, private []byte, extra ...[]byte) []byte {
	return el(idTrackEntry, num(idTrackNumber, 2), num(idTrackType, trackSubtitle), text(idCodecID, codec),
		el(idCodecPrivate, private), slices.Concat(extra...))
}

func TestStyledSubtitleHeadersAreCleaned(t *testing.T) {
	secrets := []string{
		"Aegisub", "aegisub.org", "Test Episode", "Test Author", "Test Translator", "Test Editor", "Test Timer",
		"Test Updater", "Test Details", "Test Person", "Project Garbage", "Test Storage", "Test Commenter",
		"TestEncodedFontData", "Test note", "[Fonts]",
	}
	kept := []string{
		"[Script Info]", "ScriptType: v4.00+", "WrapStyle: 0", "ScaledBorderAndShadow: yes", "YCbCr Matrix: TV.709",
		"PlayResX: 1920", "PlayResY: 1080", "[V4+ Styles]", "Format: Name, Fontname", "Style: Default,Arial",
		"[Events]", "Format: Layer, Start",
	}
	for _, codec := range []string{"S_TEXT/ASS", "S_TEXT/SSA", "S_ASS", "S_SSA"} {
		t.Run(codec, func(t *testing.T) {
			data := fileWithTrack(subtitleTrack(codec, []byte(assHeader)))
			out, rep := clean(t, data)
			for _, s := range secrets {
				if bytes.Contains(out, []byte(s)) {
					t.Errorf("%q survived", s)
				}
			}
			for _, s := range kept {
				if !bytes.Contains(out, []byte(s)) {
					t.Errorf("%q went", s)
				}
			}
			if len(out) != len(data) {
				t.Errorf("length changed from %d to %d", len(data), len(out))
			}
			if len(rep.Residual()) == 0 {
				t.Error("nothing was reported")
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "subtitle tracks") {
				t.Errorf("warnings = %q", rep.Warnings)
			}
			again, second := clean(t, out)
			if !bytes.Equal(again, out) {
				t.Error("a second clean changed the file")
			}
			if len(second.Findings) != 0 {
				t.Errorf("a second clean found %+v", second.Findings)
			}
		})
	}
}

// Parsers read a header line by line and stop at a NUL, so only blank lines may
// stand where a field was.
func TestCleanedSubtitleHeaderHasOnlyBlankLinesInPlaceOfFields(t *testing.T) {
	data := fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte(assHeader)))
	out, _ := clean(t, data)
	i := bytes.Index(out, []byte("[Script Info]"))
	if i < 0 {
		t.Fatal("no header")
	}
	header := out[i : i+len(assHeader)]
	if bytes.IndexByte(header, 0) >= 0 {
		t.Error("the header holds a NUL")
	}
	var lines []string
	for line := range strings.Lines(string(header)) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			lines = append(lines, line)
		}
	}
	want := []string{
		"[Script Info]", "ScriptType: v4.00+", "WrapStyle: 0", "ScaledBorderAndShadow: yes", "YCbCr Matrix: TV.709",
		"PlayResX: 1920", "PlayResY: 1080",
		"[V4+ Styles]", "Format: Name, Fontname, Fontsize, PrimaryColour", "Style: Default,Arial,20,&H00FFFFFF",
		"[Events]", "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q", lines)
	}
}

func TestSubtitleHeaderReportNamesPeople(t *testing.T) {
	_, rep := clean(t, fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte(assHeader))))
	var names []string
	for _, f := range rep.Findings {
		if f.Risk == core.RiskHigh && f.Category == core.CatIdentity {
			names = append(names, f.Field+"="+f.Value)
		}
	}
	for _, want := range []string{"original script=Test Author", "video file=C:/Users/Test Person/video.mkv"} {
		if !slices.ContainsFunc(names, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("no high-risk identity finding for %q in %q", want, names)
		}
	}
}

// A header that cannot be read as text, or that is stored compressed, stays. The
// user must be told it stays.
func TestUncleanableSubtitleHeadersWarn(t *testing.T) {
	compressed := el(idContentEncodings, el(0x6240, num(0x5032, 3), el(0x5034, num(0x4254, 0))))
	for name, data := range map[string][]byte{
		"UTF-16":     fileWithTrack(subtitleTrack("S_TEXT/ASS", utf16LE(assHeader))),
		"compressed": fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("x\x9c\x01\x02\x03"), compressed)),
		"too large":  fileWithTrack(subtitleTrack("S_TEXT/ASS", bytes.Repeat([]byte("Title: Test Author\n"), maxCodecHeaderRead/19+1))),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data)
			found := false
			for _, w := range rep.Warnings {
				found = found || strings.Contains(w, "could not be cleaned")
			}
			if !found {
				t.Errorf("warnings = %q", rep.Warnings)
			}
		})
	}
}

// Other subtitle codecs' private data holds text that stays, so the warning must say
// so without claiming to have cleaned it.
func TestOtherSubtitleHeadersWarn(t *testing.T) {
	data := fileWithTrack(subtitleTrack("S_TEXT/WEBVTT", []byte("WEBVTT\n\nNOTE written by Test Author\n")))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("a WebVTT header changed")
	}
	if len(rep.Warnings) != 2 {
		t.Errorf("warnings = %q", rep.Warnings)
	}
}

// Compressed private data of a codec with comment headers is not readable, so it is
// refused as before. Of other codecs it stays, and is reported.
func TestCompressedPrivateDataWarns(t *testing.T) {
	compressed := el(idContentEncodings, el(0x6240, num(0x5032, 2), el(0x5034, num(0x4254, 0))))
	data := fileWithElementInTrack(slices.Concat(compressed, el(idCodecPrivate, []byte{0x78, 0x9c, 1, 2})))
	if _, rep := clean(t, data); len(rep.Warnings) != 1 {
		t.Errorf("warnings = %q", rep.Warnings)
	}
	frames := el(idContentEncodings, el(0x6240, num(0x5032, 1), el(0x5034, num(0x4254, 3))))
	data = fileWithElementInTrack(slices.Concat(frames, el(idCodecPrivate, []byte{0x78, 0x9c, 1, 2})))
	if _, rep := clean(t, data); len(rep.Warnings) != 0 {
		t.Errorf("frame compression warned about private data: %q", rep.Warnings)
	}
}

func TestLimitationsDescribeClusters(t *testing.T) {
	joined := strings.Join(handler{}.Format().Limitations, " ")
	if strings.Contains(joined, "copied without being read") {
		t.Error("the limitations still say clusters are not read")
	}
}

// Readers go by the codec, and skip a track whose type disagrees with it, which makes its
// blocks a place to hide anything.
func TestTrackCodecMustMatchItsType(t *testing.T) {
	track := func(kind uint64, codec string) []byte {
		return fileWithTrack(el(idTrackEntry, num(idTrackNumber, 1), num(idTrackType, kind), text(idCodecID, codec)))
	}
	for _, tc := range []struct {
		kind  uint64
		codec string
	}{
		{trackVideo, "S_TEXT/UTF8"}, {trackVideo, "A_AAC"}, {trackVideo, "D_WEBVTT/SUBTITLES"}, {trackVideo, "X_TEST"},
		{trackAudio, "V_VP9"}, {trackAudio, "S_TEXT/UTF8"}, {trackSubtitle, "V_VP9"}, {trackSubtitle, "A_AAC"},
	} {
		wantError(t, track(tc.kind, tc.codec), core.ErrUnsupported)
	}
	for _, tc := range []struct {
		kind  uint64
		codec string
	}{
		{trackVideo, "V_VP9"}, {trackVideo, "V_QUICKTIME"}, {trackAudio, "A_AAC"}, {trackAudio, "A_QUICKTIME"},
		{trackSubtitle, "S_TEXT/UTF8"}, {trackSubtitle, "D_WEBVTT/SUBTITLES"},
	} {
		data := track(tc.kind, tc.codec)
		if out, _ := clean(t, data); !bytes.Equal(out, data) {
			t.Errorf("%#x %s changed", tc.kind, tc.codec)
		}
	}
}

// Even a field that stays must hold a setting, not a sentence.
func TestSubtitleSettingsHoldOnlySettings(t *testing.T) {
	for _, line := range []string{"ScriptType: " + secretText, "Language: " + secretText, "Timer: " + secretText, "PlayResX: " + secretText} {
		data := fileWithTrack(subtitleTrack("S_TEXT/ASS", []byte("[Script Info]\n"+line+"\n")))
		out, rep := clean(t, data)
		if bytes.Contains(out, []byte(secretText)) {
			t.Errorf("%q survived", line)
		}
		if len(rep.Residual()) == 0 {
			t.Errorf("%q: nothing was reported", line)
		}
	}
}

// ffmpeg plays on past a segment's declared end, so a cluster there is video, and
// treating it as stray bytes would throw it away.
func TestMediaPastTheEndOfTheSegmentIsRefused(t *testing.T) {
	doc := slices.Concat(ebmlHeader(docMatroska), el(idSegment, plainInfo(), el(idTracks, videoTrack()), cluster()))
	wantError(t, slices.Concat(doc, cluster()), core.ErrMalformed)
	wantError(t, slices.Concat(doc, cluster(), cluster()), core.ErrMalformed)
	wantError(t, slices.Concat(doc, el(idSegment, cluster())), core.ErrMalformed)
	// Other bytes there are just bytes, and go.
	for _, tail := range [][]byte{[]byte("Test Trailing Secret"), tags()} {
		out, rep := clean(t, slices.Concat(doc, tail))
		if !bytes.Equal(out, doc) || len(rep.Residual()) == 0 {
			t.Errorf("%q: output %d bytes, findings %+v", tail, len(out), rep.Findings)
		}
	}
}

// A checksum that does not match what it covers says nothing. Readers do not check it,
// so without a check here it is four bytes of anything, in every master of the file.
func TestChecksumThatDoesNotMatchGoes(t *testing.T) {
	covered := simpleBlock()
	valid := el(idCRC32, binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(covered)))
	wrong := el(idCRC32, []byte("Tst!"))

	out, rep := clean(t, clusterFile(wrong, covered))
	if bytes.Contains(out, []byte("Tst!")) {
		t.Fatal("a checksum that does not match survived")
	}
	found := false
	for _, f := range rep.Findings {
		found = found || f.Field == "Data in a checksum"
	}
	if !found {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	if out, rep := clean(t, clusterFile(valid, covered)); !bytes.Contains(out, valid) || len(rep.Findings) != 0 {
		t.Fatalf("a matching checksum went: %+v", rep.Findings)
	}
}

// Some readers match only the start of a codec ID, so a suffix is free text. It has to
// stay, as decoders need the ID, but the user must hear about it.
func TestUnknownCodecIDWarns(t *testing.T) {
	file := func(codec string) []byte {
		track := el(idTrackEntry, num(idTrackNumber, 1), num(idTrackType, trackVideo), text(idCodecID, codec))
		return slices.Concat(ebmlHeader(docMatroska), el(idSegment, el(idTracks, track)))
	}
	for _, codec := range []string{"V_VP9TstSpar", "V_MPEG4/ISO/AVCTstSpar", "V_TstSpar"} {
		t.Run(codec, func(t *testing.T) {
			_, rep := clean(t, file(codec))
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "codec ID") {
				t.Fatalf("warnings = %q", rep.Warnings)
			}
		})
	}
	for _, codec := range []string{"V_VP8", "V_VP9", "V_AV1", "V_MPEG4/ISO/AVC", "V_MPEGH/ISO/HEVC", "V_MPEG2", "V_MS/VFW/FOURCC", "V_THEORA", "V_FFV1", "V_UNCOMPRESSED"} {
		t.Run(codec, func(t *testing.T) {
			if _, rep := clean(t, file(codec)); len(rep.Warnings) != 0 {
				t.Fatalf("a false alarm: %q", rep.Warnings)
			}
		})
	}
}
