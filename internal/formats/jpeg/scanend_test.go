package jpeg

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

const scanSlackField = "Data after the compressed image"

// insertBefore puts extra in front of the first occurrence of at.
func insertBefore(t *testing.T, data, at, extra []byte) []byte {
	t.Helper()
	i := bytes.Index(data, at)
	if i < 0 {
		t.Fatalf("no % x in the fixture", at)
	}
	return slices.Concat(data[:i], extra, data[i:])
}

// refinedProgressiveJPEG builds a 16x16 gray progressive image that uses all four kinds
// of scan: DC first and refinement, AC first and refinement. The correction bits of an
// AC refinement depend on which coefficients earlier scans set, so a walk over the scan
// only stays in step if it tracks them. Every block has AC 1 and 2 set by the first AC
// scan, and the refinement adds AC 3.
func refinedProgressiveJPEG(between func(scan int) []byte) []byte {
	sos := func(ss, se, ah, al byte) []byte {
		return segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x00, ss, se, ah<<4 | al})
	}
	out := grayHeader(0xC2)
	out = append(out, dhtSegment(0, dcSymbols)...)
	out = append(out, sos(0, 0, 0, 1)...)
	dcFirst := &grayScanEncoder{}
	for block := range 4 {
		diff, _, _ := blockCoefficients(block)
		dcFirst.dc(diff)
	}
	dcFirst.flush()
	out = append(out, dcFirst.out...)
	out = append(out, between(1)...)
	out = append(out, dhtSegment(1, acSymbols)...)
	out = append(out, sos(1, 63, 0, 1)...)
	acFirst := &grayScanEncoder{}
	for block := range 4 {
		_, ac1, ac2 := blockCoefficients(block)
		acFirst.ac(ac1)
		acFirst.ac(ac2)
		acFirst.symbol(acSymbols, 0x00) // end of block
	}
	acFirst.flush()
	out = append(out, acFirst.out...)
	out = append(out, between(2)...)
	out = append(out, sos(0, 0, 1, 0)...)
	dcRefine := &grayScanEncoder{}
	for block := range 4 {
		dcRefine.bits(uint32(block&1), 1)
	}
	dcRefine.flush()
	out = append(out, dcRefine.out...)
	out = append(out, between(3)...)
	out = append(out, sos(1, 63, 1, 0)...)
	acRefine := &grayScanEncoder{}
	for block := range 4 {
		acRefine.symbol(acSymbols, 0x01)     // no zero run, then one new coefficient
		acRefine.bits(1, 1)                  // its sign
		acRefine.bits(uint32(block&1), 1)    // the correction bit for AC 1
		acRefine.bits(uint32(block>>1&1), 1) // the correction bit for AC 2
		acRefine.symbol(acSymbols, 0x00)     // end of block
	}
	acRefine.flush()
	out = append(out, acRefine.out...)
	out = append(out, between(4)...)
	return append(out, 0xFF, jpegmeta.MarkerEOI)
}

// Bytes after a scan's last block fall between the data and the next marker, so every
// decoder steps over them, and they can be as long as a writer likes. Finding them
// means walking the Huffman codes, since they look just like compressed data.
func TestBytesAfterTheLastBlockAreRemoved(t *testing.T) {
	hide := func(int) []byte { return []byte(hiddenText) }
	base := noisyJPEG(t)
	cases := map[string]struct{ data, want []byte }{
		"baseline":            {insertBefore(t, base, []byte{0xFF, jpegmeta.MarkerEOI}, []byte(hiddenText)), base},
		"progressive":         {progressiveJPEG(hide), progressiveJPEG(noBetween)},
		"refined progressive": {refinedProgressiveJPEG(hide), refinedProgressiveJPEG(noBetween)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !bytes.Contains(c.data, []byte(hiddenText)) {
				t.Fatal("the fixture holds no hidden text")
			}
			before := decode(t, c.data)
			out, rep := clean(t, c.data, core.Options{})
			assertNoHiddenText(t, out)
			if !bytes.Equal(out, c.want) {
				t.Fatalf("expected the file without the hidden bytes: got %d bytes, want %d", len(out), len(c.want))
			}
			assertSamePixels(t, before, decode(t, out))
			if !hasField(rep, scanSlackField, core.ActionRemove) {
				t.Fatalf("no finding: %+v", rep.Findings)
			}
			assertIdempotent(t, out)
		})
	}
}

// With restart intervals there is room after each interval as well as after the last.
// image/jpeg refuses such bytes where libjpeg steps over them, so decoding the output
// here also shows they have gone.
func TestBytesBeforeARestartMarkerAreRemoved(t *testing.T) {
	restart := restartJPEG(false)
	hidden := insertBefore(t, restart, []byte{0xFF, 0xD0}, []byte(hiddenText))
	hidden = insertBefore(t, hidden, []byte{0xFF, 0xD2}, []byte(hiddenText))
	hidden = insertBefore(t, hidden, []byte{0xFF, jpegmeta.MarkerEOI}, []byte(hiddenText))
	out, rep := clean(t, hidden, core.Options{})
	assertNoHiddenText(t, out)
	if !bytes.Equal(out, restart) {
		t.Fatalf("expected the file without the hidden bytes: got %d bytes, want %d", len(out), len(restart))
	}
	assertSamePixels(t, decode(t, restart), decode(t, out))
	if !hasField(rep, scanSlackField, core.ActionRemove) {
		t.Fatalf("no finding: %+v", rep.Findings)
	}
	assertIdempotent(t, out)
}

// A file that holds nothing extra comes through untouched by the walk, and padding of
// fill bytes in front of a marker, which real writers use, is not called hidden data.
func TestCompleteScansAreNotChanged(t *testing.T) {
	for name, data := range map[string][]byte{
		"baseline":            noisyJPEG(t),
		"restarts":            restartJPEG(false),
		"restarts with fill":  restartJPEG(true),
		"progressive":         progressiveJPEG(noBetween),
		"refined progressive": refinedProgressiveJPEG(noBetween),
	} {
		t.Run(name, func(t *testing.T) {
			out, rep := clean(t, data, core.Options{})
			if !bytes.Equal(out, data) {
				t.Fatal("a complete scan changed")
			}
			if hasField(rep, scanSlackField, core.ActionRemove) {
				t.Fatalf("a false alarm: %+v", rep.Findings)
			}
		})
	}
}

// When the Huffman codes run out before the picture does, what follows the walk is not
// known to be spare, so nothing may be cut: a damaged file keeps what it has.
func TestScanThatEndsEarlyIsNotCut(t *testing.T) {
	base := noisyJPEG(t)
	scan := lastScan(t, base)
	short := bytes.Replace(base, scan, scan[:len(scan)/2], 1)
	out, rep := clean(t, short, core.Options{})
	if !bytes.Contains(out, scan[:len(scan)/2]) {
		t.Fatal("part of a short scan was cut")
	}
	if hasField(rep, scanSlackField, core.ActionRemove) {
		t.Fatalf("a false alarm: %+v", rep.Findings)
	}
}

// A huge frame with almost no data must not cost much time or memory to walk, however
// many scans repeat it. Each scan's end-of-band runs cover millions of blocks with a
// few bytes.
func TestScanWalkIsBounded(t *testing.T) {
	out := []byte{0xFF, jpegmeta.MarkerSOI}
	dqt := append([]byte{0}, bytes.Repeat([]byte{4}, 64)...)
	out = append(out, segment(jpegmeta.MarkerDQT, dqt)...)
	out = append(out, segment(0xC2, []byte{8, 0x40, 0x00, 0x40, 0x00, 1, 1, 0x11, 0})...)
	out = append(out, dhtSegment(1, []byte{0x00, 0xE0, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06})...)
	sos := segment(jpegmeta.MarkerSOS, []byte{1, 1, 0x00, 1, 63, 0})
	for range 3000 {
		out = append(out, sos...)
		w := &grayScanEncoder{}
		for range 200 {
			w.bits(1, 4) // the code for symbol 0xE0, the longest run but one
			w.bits(0x3FFF, 14)
		}
		w.flush()
		out = append(out, w.out...)
	}
	out = append(out, 0xFF, jpegmeta.MarkerEOI)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var sink bytes.Buffer
		_, _ = handler{}.Clean(context.Background(), core.BytesSource(out), &sink, core.Options{})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("walking the scans took too long")
	}
}
