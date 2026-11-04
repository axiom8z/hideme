// Package samples builds one small valid file per handler, each carrying obviously
// fake metadata. It exists for the tests that cross handler boundaries, which cannot
// reach the fixtures private to each handler's own test files. Only test files import
// it, so none of this reaches the binary.
package samples

import (
	"bytes"
	"encoding/binary"
)

// Sample is a file for one handler.
type Sample struct {
	// Name is a file name with the extension a real file of this kind would have.
	Name string
	// Handler is the Format.ID of the handler that must own it.
	Handler string
	Data    []byte
	// Secrets are byte strings planted in the metadata, which must not survive.
	Secrets [][]byte
	// Content is a byte string from the file's real content, which must survive.
	Content []byte
}

// All returns a sample for every handler. A new handler with no sample here fails
// TestEveryHandlerHasASample, so this list cannot quietly fall behind the registry.
func All() []Sample {
	return []Sample{
		JPEG(), PNG(), GIF(), WebP(), TIFF(), HEIC(), MP4(), Matroska(), AVI(),
		MP3(), FLAC(), Ogg(), WAV(), AIFF(), PDF(), DOCX(), ODT(), SVG(),
	}
}

// Fake values used throughout. Handlers' reports show them, so tests can look for
// them in output as well as in cleaned bytes.
const (
	Author = "Test Author"
	Title  = "Test Title"
)

func text(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// pattern is media-like filler: it depends on its seed and holds no 0xFF, so it has
// no accidental audio frame sync and cannot be mistaken for structure.
func pattern(seed, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((seed*7 + i*13) % 251)
	}
	return out
}
