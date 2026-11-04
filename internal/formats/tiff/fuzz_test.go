package tiff

import (
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/jpegmeta"
)

func FuzzClean(f *testing.F) {
	jpegStrip := withSegments(realJPEG(f, 1), []byte("trailer"), jpegSegment(jpegmeta.MarkerCOM, []byte("comment")))
	tiled := imageSpec{width: 4, height: 4, tileWidth: 4, tileHeight: 4, parts: [][]byte{pixels(1, 16)}}
	planar := imageSpec{width: 2, height: 2, samples: 3, planar: 2, rowsPerStrip: 2, parts: [][]byte{pixels(5, 4), pixels(6, 4), pixels(7, 4)}}
	f.Add(loaded(littleEndian))
	f.Add(loaded(bigEndian))
	f.Add(singlePage(littleEndian, plainSpec(1)))
	f.Add(singlePage(bigEndian, tiled))
	f.Add(singlePage(littleEndian, planar))
	f.Add(singlePage(littleEndian, imageSpec{width: 16, height: 16, rowsPerStrip: 16, compression: compressionJPEG, photometric: 1, parts: [][]byte{jpegStrip}}))
	f.Add([]byte("II+\x00\x08\x00\x00\x00\x00\x00\x00\x00\x10\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, handler{}, data)
	})
}
