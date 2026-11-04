package avi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

const secretText = "Test Author, 1 Secret Road"

// aviFile holds the given movi children, with one video and one audio stream.
func aviFile(moviChildren ...[]byte) []byte {
	return riffChunk(formAVI,
		list("hdrl", chunk("avih", []byte("MAIN-AVI-HEADER1")), streamList(), audioStreamList()),
		list("movi", slices.Concat(moviChildren...)))
}

func audioStreamList() []byte {
	return list("strl", chunk("strh", []byte("audsSTREAM-HEADER")), chunk("strf", []byte(formatText)))
}

func frame() []byte { return chunk("00dc", []byte(frameBytes)) }

func TestMoviIsHeldToAnAllowlist(t *testing.T) {
	for name, hidden := range map[string][]byte{
		"unknown chunk":     chunk("XXXX", []byte(secretText)),
		"JUNK with data":    chunk("JUNK", []byte(secretText)),
		"INFO list":         list("INFO", chunk("IART", []byte(secretText))),
		"unknown list":      list("xxxx", chunk("00dc", []byte(secretText))),
		"undeclared stream": chunk("99dc", []byte(secretText)),
		"third stream":      chunk("02wb", []byte(secretText)),
		"idx1":              chunk("idx1", []byte(secretText)),
		"nested RIFF":       chunk("RIFF", []byte("XXXX"), []byte(secretText)),
		"unknown in rec":    list("rec ", frame(), chunk("XXXX", []byte(secretText))),
		"list in rec":       list("rec ", list("rec ", chunk("00dc", []byte(secretText)))),
		"JUNK in rec":       list("rec ", chunk("JUNK", []byte(secretText))),
	} {
		t.Run(name, func(t *testing.T) {
			data := aviFile(frame(), hidden, frame())
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
			if n := bytes.Count(out, []byte(frameBytes)); n < 2 {
				t.Errorf("%d frames survived", n)
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
	t.Run("in a second movi list", func(t *testing.T) {
		data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()),
			list("movi", frame()), list("movi", chunk("XXXX", []byte(secretText))))
		if out, _ := clean(t, data); bytes.Contains(out, []byte(secretText)) {
			t.Error("the planted text survived")
		}
	})
	t.Run("in a continuation segment", func(t *testing.T) {
		data := slices.Concat(aviFile(frame()), riffChunk(formAVIX, list("movi", frame(), chunk("XXXX", []byte(secretText)))))
		if out, _ := clean(t, data); bytes.Contains(out, []byte(secretText)) {
			t.Error("the planted text survived")
		}
	})
	t.Run("before the header list", func(t *testing.T) {
		data := riffChunk(formAVI, list("movi", frame(), chunk("XXXX", []byte(secretText))),
			list("hdrl", chunk("avih", []byte("HH")), streamList()))
		out, _ := clean(t, data)
		if bytes.Contains(out, []byte(secretText)) || !bytes.Contains(out, []byte(frameBytes)) {
			t.Error("a movi list before the header list was misjudged")
		}
	})
}

func TestMoviKeepsWhatPlayersNeed(t *testing.T) {
	data := aviFile(
		frame(),
		chunk("00db", []byte(frameBytes)),
		chunk("01wb", []byte(frameBytes)),
		chunk("00pc", []byte(frameBytes)),
		chunk("ix00", []byte{1, 2, 3, 4}),
		chunk("00ix", []byte{1, 2, 3, 4}),
		list("rec ", frame(), chunk("01wb", []byte(frameBytes))),
		chunk("JUNK", make([]byte, 16)))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("a sound movi list changed")
	}
	if len(rep.Findings) != 0 || len(rep.Warnings) != 0 {
		t.Errorf("findings %+v, warnings %q", rep.Findings, rep.Warnings)
	}
}

func TestMoviPadBytesAreZeroed(t *testing.T) {
	odd := chunk("00dc", []byte("ODD"))
	odd[len(odd)-1] = padMark
	data := aviFile(frame(), odd, frame())
	out, _ := clean(t, data)
	if bytes.IndexByte(out, padMark) >= 0 {
		t.Error("the pad byte of a movi chunk survived")
	}
	if len(out) != len(data) {
		t.Errorf("length changed from %d to %d", len(data), len(out))
	}
}

// Readers hand a declared stream's chunks of any type to its decoder, so one of an
// unusual type may be media and stays, with a warning.
func TestUnusualChunkTypesWarn(t *testing.T) {
	for _, id := range []string{"00zz", "00tx", "01dc"} {
		data := aviFile(frame(), chunk(id, []byte(secretText)))
		out, rep := clean(t, data)
		if !bytes.Equal(out, data) {
			t.Errorf("%s changed", id)
		}
		if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "unusual type") {
			t.Errorf("%s: warnings = %q", id, rep.Warnings)
		}
	}
}

func TestDataStreamsWarn(t *testing.T) {
	data := riffChunk(formAVI,
		list("hdrl", chunk("avih", []byte("HH")),
			streamList(),
			list("strl", chunk("strh", []byte("txtsSTREAM-HEADER")), chunk("strf", []byte("TEXT")))),
		list("movi", frame(), chunk("01tx", []byte("Test subtitle"))))
	out, rep := clean(t, data)
	if !bytes.Equal(out, data) {
		t.Error("a text stream changed")
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "text or data stream") {
		t.Errorf("warnings = %q", rep.Warnings)
	}
}

// Frames outside a movi list mean a size is wrong. Turning them into JUNK would
// destroy the video, while ffmpeg goes on playing such files.
func TestFramesOutsideMoviAreRefused(t *testing.T) {
	data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), list("movi"), frame(), chunk("01wb", []byte("x")))
	wantError(t, data, core.ErrMalformed)
	wantError(t, slices.Concat(aviFile(frame()), riffChunk(formAVIX, list("movi"), frame())), core.ErrMalformed)
}

func TestNoStreamsRefused(t *testing.T) {
	data := riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH"))), list("movi", frame()))
	wantError(t, data, core.ErrMalformed)
}

func TestHeaderSlackIsZeroed(t *testing.T) {
	realAvih := bytes.Repeat([]byte{0xAB}, avihLayout)
	reserved := slices.Concat(realAvih, []byte("Test Reserved 16"))
	vprp := func(fields uint32, extra string) []byte {
		out := make([]byte, 32)
		out = binary.LittleEndian.AppendUint32(out, fields)
		out = append(out, make([]byte, vprpFieldSize*int(fields))...)
		return append(out, extra...)
	}
	file := func(avih, strh, dmlh, props []byte) []byte {
		return riffChunk(formAVI,
			list("hdrl", chunk("avih", avih),
				list("strl", chunk("strh", strh), chunk("strf", []byte("FMT")), chunk("vprp", props)),
				list("odml", chunk("dmlh", dmlh))),
			list("movi", frame()))
	}
	strh := slices.Concat([]byte("vids"), make([]byte, 52))
	dmlh := binary.LittleEndian.AppendUint32(nil, 20)
	for name, data := range map[string][]byte{
		"after avih":     file(slices.Concat(realAvih, make([]byte, 16), []byte(secretText)), strh, dmlh, vprp(1, "")),
		"avih reserved":  file(reserved, strh, dmlh, vprp(1, "")),
		"after strh":     file(realAvih, slices.Concat(strh, make([]byte, 8), []byte(secretText)), dmlh, vprp(1, "")),
		"after dmlh":     file(realAvih, strh, slices.Concat(dmlh, []byte(secretText)), vprp(1, "")),
		"after vprp":     file(realAvih, strh, dmlh, vprp(1, secretText)),
		"vprp too short": file(realAvih, strh, dmlh, []byte(secretText)),
		"vprp fields":    file(realAvih, strh, dmlh, slices.Concat(vprp(99, "")[:vprpHeaderSize+2*vprpFieldSize], []byte(secretText))),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data)
			if bytes.Contains(out, []byte(secretText)) || bytes.Contains(out, []byte("Test Reserved")) {
				t.Error("the planted text survived")
			}
			if len(rep.Residual()) == 0 {
				t.Error("nothing was reported")
			}
			if len(out) != len(data) {
				t.Errorf("length changed from %d to %d", len(data), len(out))
			}
			if !bytes.Contains(out, realAvih) {
				t.Error("the meaningful part of avih changed")
			}
			if again, _ := clean(t, out); !bytes.Equal(again, out) {
				t.Error("a second clean changed the file")
			}
		})
	}
	t.Run("sound headers stay", func(t *testing.T) {
		data := file(slices.Concat(realAvih, make([]byte, 16)), strh, slices.Concat(dmlh, make([]byte, 244)), vprp(2, ""))
		out, rep := clean(t, data)
		if !bytes.Equal(out, data) || len(rep.Findings) != 0 {
			t.Errorf("sound headers changed: %+v", rep.Findings)
		}
	})
}

func TestReadableTextInKeptChunksWarns(t *testing.T) {
	withStrf := func(strf []byte) []byte {
		return riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")),
			list("strl", chunk("strh", []byte("vidsSTREAM-HEADER")), chunk("strf", strf))), list("movi", frame()))
	}
	withIndx := func(indx []byte) []byte {
		return riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")),
			list("strl", chunk("strh", []byte("vidsSTREAM-HEADER")), chunk("strf", []byte{1, 2}), chunk("indx", indx))), list("movi", frame()))
	}
	withTop := func(id string, payload []byte) []byte {
		return riffChunk(formAVI, list("hdrl", chunk("avih", []byte("HH")), streamList()), list("movi", frame()), chunk(id, payload))
	}
	spanning := slices.Concat(make([]byte, zeroScanBuffer-4), []byte(secretText))
	utf16 := make([]byte, 0, 2*len(secretText))
	for _, c := range []byte(secretText) {
		utf16 = append(utf16, c, 0)
	}
	for name, data := range map[string][]byte{
		"strf":             withStrf([]byte(secretText)),
		"strf in UTF-16":   withStrf(utf16),
		"indx":             withIndx([]byte(secretText)),
		"ix00 over a read": withTop("ix00", spanning),
		"ix00":             withTop("ix00", []byte(secretText)),
		"ix00 in movi":     aviFile(chunk("ix00", []byte(secretText))),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data)
			if !bytes.Equal(out, data) {
				t.Error("a kept chunk changed")
			}
			if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "readable text") {
				t.Errorf("warnings = %q", rep.Warnings)
			}
		})
	}
	t.Run("a real index does not warn", func(t *testing.T) {
		if _, rep := clean(t, withTop("idx1", goodIndex())); len(rep.Warnings) != 0 {
			t.Errorf("warnings = %q", rep.Warnings)
		}
	})
}

func TestLimitationsDescribeMovi(t *testing.T) {
	joined := strings.Join(handler{}.Format().Limitations, " ")
	if strings.Contains(joined, "copied without being read") {
		t.Error("the limitations still say the movi list is not read")
	}
}

// ffmpeg plays all of these to the end. Treating what follows the RIFF chunk as stray
// bytes would throw away half the video and report only some "trailing data".
func TestMediaPastTheEndOfTheFileIsRefused(t *testing.T) {
	whole := aviFile(frame())
	for name, data := range map[string][]byte{
		"two files joined":    slices.Concat(whole, whole),
		"a further movi list": slices.Concat(whole, list("movi", frame())),
		"loose frames":        slices.Concat(whole, frame(), frame()),
		"after a second part": slices.Concat(whole, riffChunk(formAVIX, list("movi", frame())), frame()),
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, data, core.ErrUnsupported)
		})
	}
	// Other bytes there are just bytes, and go.
	for _, tail := range []string{"Test Trailing Secret", "LIST\x10\x00\x00\x00INFOxxxxxxxxxxxx", "RIF", "\x00\x00\x00\x00"} {
		out, rep := clean(t, slices.Concat(whole, []byte(tail)))
		if !bytes.Equal(out, whole) || len(rep.Residual()) == 0 {
			t.Errorf("%q: output %d bytes, findings %+v", tail, len(out), rep.Findings)
		}
	}
}
