package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// residueSignatures are byte strings no cleaned file should contain. They deliberately
// do not depend on any format's structure, so a parser bug that makes a handler blind
// to a block cannot also blind this scan.
//
// It cannot see into compressed data, such as a PDF's Flate streams or the parts of an
// Office document; there the handler's own Inspect is the check.
var residueSignatures = buildSignatures()

type signature struct {
	name  string
	bytes []byte
}

func buildSignatures() []signature {
	sigs := []signature{
		{"<x:xmpmeta", []byte("<x:xmpmeta")},
		{"<?xpacket", []byte("<?xpacket")},
		{"ns.adobe.com/xap", []byte("ns.adobe.com/xap")},
		{"Photoshop 3.0", []byte("Photoshop 3.0")},
	}
	// XMP may be stored in UTF-16, and then none of the bytes above appear.
	for _, s := range []string{"<x:xmpmeta", "<?xpacket"} {
		sigs = append(sigs,
			signature{s + " (UTF-16LE)", utf16Bytes(s, 0)},
			signature{s + " (UTF-16BE)", utf16Bytes(s, 1)},
		)
	}
	return sigs
}

// utf16Bytes widens ASCII, with the character at index pos of each pair.
func utf16Bytes(s string, pos int) []byte {
	out := make([]byte, 2*len(s))
	for i := range len(s) {
		out[2*i+pos] = s[i]
	}
	return out
}

// residueBufferSize is large enough that the overlap between reads is negligible.
const residueBufferSize = 1 << 20

// residueHit is the first place a signature was found.
type residueHit struct {
	name   string
	offset int64
}

// scanResidue searches r for every signature using memory that does not depend on
// the size of the file.
func scanResidue(ctx context.Context, r io.Reader) ([]residueHit, error) {
	return scanResidueBuffer(ctx, r, make([]byte, residueBufferSize))
}

// scanResidueBuffer does the work with a given buffer, which must be longer than
// the longest signature. Tests use tiny ones to force matches across every possible
// boundary. Between reads the last overlap bytes stay in the buffer, so a signature
// straddling two reads is seen whole. One byte less than the longest signature is
// exactly enough: anything longer than the overlap has already been searched whole.
func scanResidueBuffer(ctx context.Context, r io.Reader, buf []byte) ([]residueHit, error) {
	overlap := 0
	for _, s := range residueSignatures {
		overlap = max(overlap, len(s.bytes)-1)
	}
	found := make([]bool, len(residueSignatures))
	var hits []residueHit
	var base int64 // the file offset of buf[0]
	filled := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(buf[filled:])
		window := buf[:filled+n]
		for i, s := range residueSignatures {
			if found[i] {
				continue
			}
			if at := bytes.Index(window, s.bytes); at >= 0 {
				found[i] = true
				hits = append(hits, residueHit{s.name, base + int64(at)})
			}
		}
		if err == io.EOF {
			return hits, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		keep := min(overlap, len(window))
		copy(buf, window[len(window)-keep:])
		base += int64(len(window) - keep)
		filled = keep
	}
}

// describeHits names the first few. A file with one signature usually has several,
// and the message must stay short enough for core.SafeMessage not to cut it off.
func describeHits(hits []residueHit) string {
	const shown = 2
	parts := make([]string, 0, shown+1)
	for _, h := range hits[:min(len(hits), shown)] {
		parts = append(parts, fmt.Sprintf("%s at byte %d", h.name, h.offset))
	}
	if more := len(hits) - shown; more > 0 {
		parts = append(parts, fmt.Sprintf("%d more", more))
	}
	return strings.Join(parts, ", ")
}
