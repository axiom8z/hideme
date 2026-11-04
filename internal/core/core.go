// Package core defines the contract between the HideMe engine and its format
// handlers, plus the small helpers every handler needs.
//
// It imports nothing from the rest of the project, so every other package can
// depend on it without cycles. See docs/DESIGN.md for the rules handlers follow.
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Errors returned by handlers. Wrap them with %w so callers can use errors.Is.
var (
	// ErrUnsupported means the file is not, or not a supported variant of, a
	// format HideMe can clean. The file is never reported as clean.
	ErrUnsupported = errors.New("unsupported format")
	// ErrMalformed means the file could not be parsed with enough confidence
	// to guarantee its metadata was found. HideMe fails closed on this.
	ErrMalformed = errors.New("malformed file")
	// ErrEncrypted means the content is encrypted and cannot be rewritten.
	ErrEncrypted = errors.New("encrypted file")
	// ErrTooLarge means a size or expansion limit was hit.
	ErrTooLarge = errors.New("file exceeds a safety limit")
)

// Malformedf returns an error wrapping ErrMalformed.
func Malformedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// Unsupportedf returns an error wrapping ErrUnsupported.
func Unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

// Source is random-access input. *io.SectionReader satisfies it.
type Source interface {
	io.ReaderAt
	Size() int64
}

// BytesSource wraps a byte slice as a Source.
func BytesSource(b []byte) Source {
	return io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b)))
}

// Family groups formats for display.
type Family string

const (
	FamilyImage    Family = "image"
	FamilyVideo    Family = "video"
	FamilyAudio    Family = "audio"
	FamilyDocument Family = "document"
)

// Format describes a handler's file type. One handler may register several
// formats' worth of extensions but reports the specific ID it detected in
// Report.Format.
type Format struct {
	ID         string   // stable short identifier, e.g. "jpeg"
	Name       string   // human name, e.g. "JPEG image"
	Family     Family   // display grouping
	Extensions []string // lowercase, with dot, e.g. ".jpg"
	// Limitations lists, in plain language, what this handler cannot remove,
	// such as data inside the encoded picture or audio stream. It is shown by
	// `hideme formats` and the README. Honesty here is a feature.
	Limitations []string
	// ResidueWarnOnly makes the engine's raw-byte residue scan warn instead of
	// fail. Set it only for formats whose visible text could legitimately
	// contain a signature such as "<x:xmpmeta" (SVG, for example).
	ResidueWarnOnly bool
}

// Options control how aggressively a handler cleans.
type Options struct {
	// Strict also removes data that is not needed to decode the file but that
	// changes how it looks, such as ICC colour profiles and orientation.
	Strict bool
	// Depth is the current nesting level. It is managed by CleanBytes.
	Depth int
}

// Handler reads and cleans one family of file formats.
//
// Handlers must be safe for concurrent use, must never panic on any input, and
// must treat every length and offset in a file as hostile.
type Handler interface {
	// Format returns static information about the handler.
	Format() Format
	// Match reports whether the file is one this handler owns. head holds the
	// first bytes of the file (up to HeadSize); src is available for formats
	// that must look further, such as ZIP-based ones. Match decides from
	// content only, never from the file name, and must not match files that
	// another handler owns. It must not return an error: anything that cannot
	// be read simply does not match.
	Match(head []byte, src Source) bool
	// Inspect lists the metadata in src without changing anything. The Action
	// of each finding says what Clean would do with it under opts.
	Inspect(ctx context.Context, src Source, opts Options) (*Report, error)
	// Clean writes a metadata-free copy of src to dst and reports what it did.
	// On error, anything already written to dst is discarded by the caller.
	// Clean must be deterministic (same input and options give the same
	// bytes) and idempotent (cleaning its own output changes nothing).
	Clean(ctx context.Context, src Source, dst io.Writer, opts Options) (*Report, error)
}

// Category says what kind of information a finding exposes.
type Category string

const (
	CatLocation   Category = "location"   // GPS coordinates, place names
	CatDevice     Category = "device"     // camera, phone, serial numbers, printers
	CatIdentity   Category = "identity"   // names, usernames, emails, file paths
	CatTime       Category = "time"       // creation, edit and capture times
	CatSoftware   Category = "software"   // application and version
	CatThumbnail  Category = "thumbnail"  // previews, which may show pre-edit content
	CatHidden     Category = "hidden"     // trailing data, old revisions, attachments
	CatContent    Category = "content"    // titles, descriptions, comments, keywords
	CatTechnical  Category = "technical"  // colour profiles, orientation
	CatFilesystem Category = "filesystem" // alternate streams, xattrs, file times
)

// Risk says how much a finding could reveal about the person.
type Risk string

const (
	RiskHigh Risk = "high"   // locates or names someone: GPS, author, serial number
	RiskMed  Risk = "medium" // narrows someone down: device model, capture time
	RiskLow  Risk = "low"    // mild fingerprint: software version, title
	RiskNone Risk = "none"   // not identifying; technical data
)

// Action says what HideMe does with a finding.
type Action string

const (
	ActionRemove    Action = "remove"    // deleted or blanked
	ActionAnonymize Action = "anonymize" // replaced with a neutral placeholder
	ActionRetain    Action = "retain"    // deliberately kept; see Reason
)

// Finding is one piece of metadata.
type Finding struct {
	Category Category `json:"category"`
	Risk     Risk     `json:"risk"`
	Field    string   `json:"field"`           // e.g. "GPS coordinates"
	Value    string   `json:"value,omitempty"` // sanitised display value
	Where    string   `json:"where,omitempty"` // e.g. "APP1/Exif/GPS"
	Action   Action   `json:"action"`
	Reason   string   `json:"reason,omitempty"` // why it was retained
}

// Report is what a handler found or did.
type Report struct {
	Format   string    `json:"format"` // Format ID actually detected
	Findings []Finding `json:"findings"`
	Warnings []string  `json:"warnings,omitempty"` // things HideMe could not clean
	// MayLoseContent means the output may lack content the original holds, such as
	// frames stored where no tag refers to them, and the handler cannot tell. The
	// engine then refuses to replace the original unless told to. Set it only together
	// with a warning, and in both Inspect and Clean.
	MayLoseContent bool `json:"may_lose_content,omitempty"`
}

// Add appends a finding, sanitising untrusted text first.
func (r *Report) Add(f Finding) {
	f.Field = SafeText(f.Field)
	f.Value = SafeText(f.Value)
	f.Where = SafeText(f.Where)
	f.Reason = SafeText(f.Reason)
	r.Findings = append(r.Findings, f)
}

// Warn appends a warning, sanitising untrusted text first.
func (r *Report) Warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, SafeMessage(fmt.Sprintf(format, args...)))
}

// Merge folds a nested report (such as an image inside a document) into r,
// prefixing each location with where.
func (r *Report) Merge(where string, nested *Report) {
	if nested == nil {
		return
	}
	for _, f := range nested.Findings {
		if f.Where == "" {
			f.Where = where
		} else {
			f.Where = where + "/" + f.Where
		}
		r.Add(f)
	}
	for _, w := range nested.Warnings {
		r.Warn("%s: %s", where, w)
	}
	r.MayLoseContent = r.MayLoseContent || nested.MayLoseContent
}

// Residual returns the findings that were not deliberately retained. After a
// successful Clean, Inspect on the output must return none.
func (r *Report) Residual() []Finding {
	if r == nil {
		return nil
	}
	var out []Finding
	for _, f := range r.Findings {
		if f.Action != ActionRetain {
			out = append(out, f)
		}
	}
	return out
}

const (
	// maxSafeText bounds how much of any one metadata value is ever displayed.
	maxSafeText = 200
	// maxSafeMessage bounds an explanation of why a file failed. Those say what
	// happened and what to do about it, so they run longer than a value.
	maxSafeMessage = 600
)

// SafeText makes untrusted metadata safe to print. It drops invalid UTF-8 and
// every control character, including the ESC that starts terminal escape
// sequences, and truncates long values. A file must not be able to rewrite the
// user's terminal or forge HideMe's own output by way of its metadata.
func SafeText(s string) string { return safeText(s, maxSafeText) }

// SafeMessage is SafeText for an explanation rather than a value: the same filtering
// with room for a whole sentence or two.
func SafeMessage(s string) string { return safeText(s, maxSafeMessage) }

func safeText(s string, limit int) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r == utf8.RuneError && size == 1 {
			continue
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.In(r, unicode.Zl, unicode.Zp):
			r = ' ' // U+2028 and U+2029 are line breaks that IsControl does not cover
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue // includes bidi overrides, which can disguise text
		}
		if n == limit {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// ByteCount words a size for a report.
func ByteCount[T int | int64](n T) string {
	if n == 1 {
		return "1 byte"
	}
	return fmt.Sprintf("%d bytes", n)
}

// AllZero reports whether b holds only zero bytes.
func AllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// maxDecodedText bounds how much of one value DecodeText decodes. SafeText shows only
// a couple of hundred characters of it anyway, and each byte of Latin-1 costs four
// while decoding, so a huge value must not be decoded whole.
const maxDecodedText = 4096

// DecodeText turns free text of unknown encoding, such as a JPEG or GIF comment, into
// UTF-8. Latin-1 cannot fail and is what older tools meant, so anything that is not
// valid UTF-8 reads as that.
func DecodeText(b []byte) string {
	valid := utf8.Valid(b)
	b = b[:min(len(b), maxDecodedText)]
	if valid {
		return string(b)
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// HeadSize is how many leading bytes Match receives.
const HeadSize = 64 * 1024

// MaxInMemory limits files that handlers must load whole. Container formats
// that can be gigabytes long (video, audio) must stream instead.
const MaxInMemory = int64(1) << 30 // 1 GiB

// MaxNestedDepth limits recursion through embedded files.
const MaxNestedDepth = 4

// ReadAll loads src into memory, enforcing MaxInMemory.
func ReadAll(src Source) ([]byte, error) {
	if src.Size() > MaxInMemory {
		return nil, fmt.Errorf("%w: %d bytes is too large to process in memory", ErrTooLarge, src.Size())
	}
	buf := make([]byte, src.Size())
	if _, err := io.ReadFull(io.NewSectionReader(src, 0, src.Size()), buf); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return buf, nil
}

const copyBufferSize = 1 << 20

// CopyRange copies n bytes starting at off from src to dst, honouring ctx.
func CopyRange(ctx context.Context, dst io.Writer, src Source, off, n int64) error {
	if off < 0 || n < 0 || off > src.Size() || n > src.Size()-off {
		return Malformedf("range %d+%d is outside the file", off, n)
	}
	buf := make([]byte, min(n, copyBufferSize))
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf[:min(n, int64(len(buf)))]
		if _, err := io.ReadFull(io.NewSectionReader(src, off, int64(len(chunk))), chunk); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if _, err := dst.Write(chunk); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		off += int64(len(chunk))
		n -= int64(len(chunk))
	}
	return nil
}

// WriteZeros writes n zero bytes to dst, honouring ctx.
func WriteZeros(ctx context.Context, dst io.Writer, n int64) error {
	if n < 0 {
		return Malformedf("negative length %d", n)
	}
	buf := make([]byte, min(n, copyBufferSize))
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf[:min(n, int64(len(buf)))]
		if _, err := dst.Write(chunk); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		n -= int64(len(chunk))
	}
	return nil
}

// IsZeroRange reports whether the n bytes at off are all zero, honouring ctx. It reads
// through buf, which must not be empty when n is above zero, so that a region of any
// size costs no more memory than that.
func IsZeroRange(ctx context.Context, src Source, off, n int64, buf []byte) (bool, error) {
	if off < 0 || n < 0 || off > src.Size() || n > src.Size()-off {
		return false, Malformedf("range %d+%d is outside the file", off, n)
	}
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		chunk := buf[:min(n, int64(len(buf)))]
		if _, err := io.ReadFull(io.NewSectionReader(src, off, int64(len(chunk))), chunk); err != nil {
			return false, fmt.Errorf("read: %w", err)
		}
		if !AllZero(chunk) {
			return false, nil
		}
		off += int64(len(chunk))
		n -= int64(len(chunk))
	}
	return true, nil
}

// Patch replaces the Len bytes at Off in the source. If Zero is true they
// become Len zero bytes; otherwise they become Data, which may be any length.
type Patch struct {
	Off  int64
	Len  int64
	Data []byte
	Zero bool
}

// CopyPatched streams src to dst with patches applied. Patches must be sorted
// by Off and must not overlap. This is how multi-gigabyte containers are
// cleaned without loading them: offsets stay valid when each patch keeps the
// region's size, and the media between patches is copied untouched.
func CopyPatched(ctx context.Context, dst io.Writer, src Source, patches []Patch) error {
	var pos int64
	for i, p := range patches {
		if p.Off < pos || p.Len < 0 {
			return Malformedf("patch %d overlaps or is out of order", i)
		}
		if p.Off > src.Size() || p.Len > src.Size()-p.Off {
			return Malformedf("patch %d extends past the end of the file", i)
		}
		if err := CopyRange(ctx, dst, src, pos, p.Off-pos); err != nil {
			return err
		}
		if p.Zero {
			if err := WriteZeros(ctx, dst, p.Len); err != nil {
				return err
			}
		} else if _, err := dst.Write(p.Data); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		pos = p.Off + p.Len
	}
	return CopyRange(ctx, dst, src, pos, src.Size()-pos)
}

var (
	registryMu sync.RWMutex
	registry   []Handler
)

// Register adds a handler. Call it from the handler package's init function;
// internal/formats/all imports every handler package for its side effect.
func Register(h Handler) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, h)
}

// Handlers returns every registered handler.
func Handlers() []Handler {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]Handler(nil), registry...)
}

// Detect finds the handler for src by content. It returns ErrUnsupported if no
// handler matches.
func Detect(src Source) (Handler, error) {
	head := make([]byte, min(src.Size(), HeadSize))
	if _, err := src.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read: %w", err)
	}
	for _, h := range Handlers() {
		if h.Match(head, src) {
			return h, nil
		}
	}
	return nil, Unsupportedf("no handler recognises this file's content")
}

// CleanBytes detects and cleans a file embedded in another file, such as an
// image inside a document, and returns the cleaned bytes. It returns
// ErrUnsupported if the bytes are no known format. The caller decides whether
// that is acceptable (normally: keep the bytes and add a warning).
func CleanBytes(ctx context.Context, data []byte, opts Options) ([]byte, *Report, error) {
	if opts.Depth >= MaxNestedDepth {
		return nil, nil, fmt.Errorf("%w: files nested deeper than %d levels", ErrTooLarge, MaxNestedDepth)
	}
	opts.Depth++
	src := BytesSource(data)
	h, err := Detect(src)
	if err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	out.Grow(len(data))
	rep, err := h.Clean(ctx, src, &out, opts)
	if err != nil {
		return nil, nil, err
	}
	return out.Bytes(), rep, nil
}

// InspectBytes is CleanBytes' read-only counterpart.
func InspectBytes(ctx context.Context, data []byte, opts Options) (*Report, error) {
	if opts.Depth >= MaxNestedDepth {
		return nil, fmt.Errorf("%w: files nested deeper than %d levels", ErrTooLarge, MaxNestedDepth)
	}
	opts.Depth++
	src := BytesSource(data)
	h, err := Detect(src)
	if err != nil {
		return nil, err
	}
	return h.Inspect(ctx, src, opts)
}
