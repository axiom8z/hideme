package webp

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
)

const hiddenText = "Test Hidden Text"

// clean prints Field and Where and leaves Value out, so a chunk's name, which any
// file can set, belongs in Value alone.
func TestFileTextStaysOutOfLabels(t *testing.T) {
	const marker = "MRKR"
	vp8 := chunk(idVP8, fakeVP8("x"))
	other := chunk(marker, []byte("x"))
	for name, data := range map[string][]byte{
		"after a simple image": file(vp8, other),
		"in an extended file":  file(vp8x(0), other, vp8),
		"in an animation":      file(vp8x(flagAnimation), anim(), frame(0, other, vp8)),
	} {
		t.Run(name, func(t *testing.T) {
			_, rep := clean(t, data, core.Options{})
			if !hasField(rep, "Other chunk", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			for _, f := range rep.Findings {
				if strings.Contains(f.Field, marker) || strings.Contains(f.Where, marker) || strings.Contains(f.Reason, marker) {
					t.Errorf("finding %+v repeats text from the file outside Value", f)
				}
			}
		})
	}
}

func TestFileTextStaysOutOfErrors(t *testing.T) {
	const marker = "MRKR"
	var out bytes.Buffer
	_, err := handler{}.Clean(context.Background(), core.BytesSource(file(chunk(marker, []byte("x")))), &out, core.Options{})
	if err == nil {
		t.Fatal("the file should be refused")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("the error %q repeats text from the file", err)
	}
}

// An uncompressed alpha plane has one byte per pixel and decoders read no more, so
// anything after that is no part of the picture.
func TestDataAfterTheAlphaPlaneIsRemoved(t *testing.T) {
	const width, height = 16, 16
	plane := append([]byte{0}, bytes.Repeat([]byte{0xFF}, width*height)...)
	vp8 := chunk(idVP8, fakeVP8("x"))
	for name, build := range map[string]func(alpha []byte) []byte{
		"still image": func(alpha []byte) []byte { return file(vp8x(flagAlpha), chunk(idALPH, alpha), vp8) },
		"animation frame": func(alpha []byte) []byte {
			return file(vp8x(flagAnimation|flagAlpha), anim(), frame(0, chunk(idALPH, alpha), vp8))
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, build(slices.Concat(plane, []byte(hiddenText))), core.Options{})
			if bytes.Contains(out, []byte(hiddenText)) {
				t.Fatal("the hidden text survived cleaning")
			}
			if want := build(plane); !bytes.Equal(out, want) {
				t.Fatalf("got %d bytes, want %d", len(out), len(want))
			}
			if !hasField(rep, "Data after the alpha plane", core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			if again, rep := clean(t, out, core.Options{}); !bytes.Equal(again, out) || len(rep.Findings) != 0 {
				t.Fatalf("not idempotent: %+v", rep.Findings)
			}
		})
	}
	t.Run("an exact plane is not touched", func(t *testing.T) {
		data := file(vp8x(flagAlpha), chunk(idALPH, plane), vp8)
		if out, rep := clean(t, data, core.Options{}); !bytes.Equal(out, data) || len(rep.Findings) != 0 {
			t.Fatalf("findings: %+v", rep.Findings)
		}
	})
	t.Run("a compressed plane cannot be measured", func(t *testing.T) {
		compressed := append([]byte{1}, bytes.Repeat([]byte{0xAB}, 1000)...)
		data := file(vp8x(flagAlpha), chunk(idALPH, compressed), vp8)
		if out, _ := clean(t, data, core.Options{}); !bytes.Equal(out, data) {
			t.Fatal("it must be copied as it is")
		}
	})
}
