// Package audiotag decodes the tag formats that audio containers have in common:
// ID3v2, ID3v1, Enhanced TAG, APEv1/v2, Lyrics3 and Vorbis comments.
//
// It exists because taggers do not respect container boundaries: an ID3v2 tag
// turns up in front of a FLAC file and inside a WAV chunk, and an APE tag after
// an Ogg stream. It is not a handler, so handlers can share it without importing
// one another. It lives under mp3 because MP3 is where these tags come from.
//
// Everything here is for the report. Handlers remove a tag by its extent whether
// or not it decoded, so a damaged tag costs detail, never privacy, and the
// decoders report damage instead of failing.
package audiotag

import (
	"fmt"
	"io"

	"github.com/axiom8z/hideme/internal/core"
)

// windowSize is how much a Window reads ahead. Frame and page walks touch every
// few hundred bytes of a file, so reading ahead saves a system call for each one.
const windowSize = 256 << 10

// Window gives cheap random access to small pieces of a Source through a single
// reused buffer. A multi-gigabyte recording costs the same memory as a short one.
//
// Read failures are sticky rather than returned from each call, because the walk
// loops read at nearly every step. A loop must check Err before it trusts a nil
// result to mean "past the end": mistaking a failed read for the end of the file
// would silently shorten the output.
type Window struct {
	src  core.Source
	size int64
	buf  []byte
	off  int64
	err  error
}

// NewWindow returns a Window over src.
func NewWindow(src core.Source) *Window {
	return &Window{src: src, size: src.Size()}
}

// Source returns the underlying source.
func (w *Window) Source() core.Source { return w.src }

// Size returns the size of the underlying source.
func (w *Window) Size() int64 { return w.size }

// Err returns the first read failure, if any.
func (w *Window) Err() error { return w.err }

// At returns the n bytes at off, or nil if they are not all inside the file. The
// slice is valid until the next call. Callers bound n; the buffer grows to fit.
func (w *Window) At(off int64, n int) []byte {
	if off < 0 || n < 0 || off > w.size || int64(n) > w.size-off {
		return nil
	}
	if off >= w.off && off+int64(n) <= w.off+int64(len(w.buf)) {
		return w.buf[off-w.off:][:n]
	}
	if !w.fill(off, n) {
		return nil
	}
	return w.buf[:n]
}

// AtMost is At for callers that can use a short read at the end of the file.
func (w *Window) AtMost(off int64, n int) []byte {
	if off < 0 || off > w.size {
		return nil
	}
	return w.At(off, int(min(int64(n), w.size-off)))
}

// From returns the buffered bytes starting at off, reading if off is not
// buffered. It returns nil at or past the end of the file. Loops over a long
// region use it to process a buffer at a time.
func (w *Window) From(off int64) []byte {
	if off < 0 || off >= w.size {
		return nil
	}
	if off >= w.off && off < w.off+int64(len(w.buf)) {
		return w.buf[off-w.off:]
	}
	if !w.fill(off, 1) {
		return nil
	}
	return w.buf
}

func (w *Window) fill(off int64, n int) bool {
	want := min(int64(max(n, windowSize)), w.size-off)
	if int64(cap(w.buf)) < want {
		w.buf = make([]byte, want)
	}
	w.buf = w.buf[:want]
	w.off = off
	if read, err := w.src.ReadAt(w.buf, off); int64(read) != want {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		if w.err == nil {
			w.err = fmt.Errorf("read %d bytes at offset %d: %w", want, off, err)
		}
		w.buf = w.buf[:0]
		return false
	}
	return true
}
