package office

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/axiom8z/hideme/internal/core"
)

// limits bound how much a document may expand. They are a field of the handlers
// so that tests can exercise them without decompressing gigabytes.
type limits struct {
	maxEntry int64 // decompressed bytes in one entry
	maxTotal int64 // decompressed bytes in the document and everything nested in it

	// maxExpansion, when set, lowers maxTotal to this many times the size of the file,
	// though never below minTotal, which is there for small documents.
	maxExpansion int64
	minTotal     int64
}

// defaultLimits come from measuring. Random numbers in a sheet compress 5 times, the
// same value in every cell 14, the same formula 18. Going past 300 takes content that
// repeats perfectly, which is what bombs are made of, and deflate itself stops at 1032.
// So 128 leaves real spreadsheets a wide margin, while a 255 KB file can no longer ask
// for 250 MB. Below 250 KB the floor applies: small, repetitive documents still pass.
var defaultLimits = limits{maxEntry: core.MaxInMemory, maxTotal: 4 << 30, maxExpansion: 128, minTotal: 32 << 20}

// totalFor returns the budget for a document of this size.
func (l limits) totalFor(size int64) int64 {
	if l.maxExpansion <= 0 || size > l.maxTotal/l.maxExpansion {
		return l.maxTotal
	}
	return min(l.maxTotal, max(l.minTotal, size*l.maxExpansion))
}

const (
	// maxEntries is the classic ZIP limit; no real document comes near it.
	maxEntries = 65535
	readChunk  = 1 << 20

	// msdosEpoch is 1980-01-01 in MS-DOS date format, the earliest a ZIP can say.
	msdosEpoch = 0x0021

	zip64ExtraID = 0x0001
)

type budgetKey struct{}

// budget is how much more a document may decompress.
type budget struct {
	left  atomic.Int64
	total int64
}

// withBudget returns the decompression budget for this document, set by the size of
// the outermost file. The budget travels in the context because core.CleanBytes calls
// straight back into a handler for nested documents. Each level alone could be under
// its limits while a document of documents of documents still expands without bound.
func withBudget(ctx context.Context, lim limits, size int64) (context.Context, *budget) {
	if b, ok := ctx.Value(budgetKey{}).(*budget); ok {
		return ctx, b
	}
	b := &budget{total: lim.totalFor(size)}
	b.left.Store(b.total)
	return context.WithValue(ctx, budgetKey{}, b), b
}

// archive is a validated ZIP file.
type archive struct {
	zr      *zip.Reader
	src     core.Source
	lim     limits
	budget  *budget
	entries []*zip.File
	byKey   map[string]*zip.File
	index   map[*zip.File]int
}

// openArchive reads the central directory and refuses anything whose entries
// cannot be trusted. It returns a context carrying the decompression budget.
func openArchive(ctx context.Context, src core.Source, lim limits) (context.Context, *archive, error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	zr, err := zip.NewReader(src, src.Size())
	if err != nil {
		return ctx, nil, core.Malformedf("ZIP archive: %v", err)
	}
	if len(zr.File) > maxEntries {
		return ctx, nil, core.Malformedf("ZIP archive has %d entries, more than the limit of %d", len(zr.File), maxEntries)
	}
	ctx, budget := withBudget(ctx, lim, src.Size())
	a := &archive{zr: zr, src: src, lim: lim, budget: budget, entries: zr.File, byKey: make(map[string]*zip.File, len(zr.File)), index: make(map[*zip.File]int, len(zr.File))}
	for i, f := range zr.File {
		if err := checkEntry(f); err != nil {
			return ctx, nil, err
		}
		a.index[f] = i
		key := partKey(f.Name)
		if _, dup := a.byKey[key]; dup {
			return ctx, nil, core.Malformedf("ZIP archive has more than one entry named %q", f.Name)
		}
		a.byKey[key] = f
	}
	return ctx, a, nil
}

// checkEntry rejects what would make the entry's name or contents unreliable.
func checkEntry(f *zip.File) error {
	if !validEntryName(f.Name) {
		return core.Malformedf("ZIP entry name %q is not valid in a document", f.Name)
	}
	// Neither format uses ZIP encryption, so this is both encrypted and not a valid
	// document. The error says both, and the engine can report whichever it prefers.
	if f.Flags&0x1 != 0 {
		return fmt.Errorf("%w: %w: ZIP entry %q is encrypted", core.ErrEncrypted, core.ErrMalformed, f.Name)
	}
	if f.Method != zip.Store && f.Method != zip.Deflate {
		return core.Unsupportedf("ZIP entry %q uses compression method %d", f.Name, f.Method)
	}
	if strings.HasSuffix(f.Name, "/") && f.UncompressedSize64 != 0 {
		return core.Malformedf("ZIP directory entry %q holds data", f.Name)
	}
	return nil
}

// validEntryName refuses names no document writer produces. A backslash or a dot
// segment matters most: a name like docProps\core.xml would dodge every check on
// part names while still carrying its data into the output.
func validEntryName(name string) bool {
	if name == "" || name[0] == '/' || strings.ContainsFunc(name, func(r rune) bool {
		return r == '\\' || unicode.IsControl(r)
	}) {
		return false
	}
	for segment := range strings.SplitSeq(strings.TrimSuffix(name, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// partKey is the identity of a part. OPC part names are case-insensitive and are
// percent-encoded in ZIP item names, so these all name one part and must all be
// recognised as it: docProps/core.xml, DOCPROPS/CORE.XML, docProps/c%6Fre.xml.
func partKey(name string) string {
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	return strings.ToLower(name)
}

// read decompresses an entry, counting the bytes actually produced. The size the ZIP
// declares is never believed, but it is worth reserving as far as the budget allows: a
// buffer left to double its way up allocates several times the part, while a lie gains
// nothing, since the ZIP reader fails any entry not exactly the size it declared.
func (a *archive) read(ctx context.Context, f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, entryError(f, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if room := min(a.budget.left.Load(), a.lim.maxEntry); room > 0 {
		if f.UncompressedSize64 < uint64(room) {
			room = int64(f.UncompressedSize64)
		}
		buf.Grow(int(room) + 2*bytes.MinRead) // a Buffer reads only with this much spare
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Never past the first byte over a limit: the buffer holds only what was reserved,
		// and reading further would double it just before being refused.
		chunk := min(readChunk, max(a.budget.left.Load(), 0)+1, max(a.lim.maxEntry-int64(buf.Len()), 0)+1)
		n, err := io.CopyN(&buf, rc, chunk)
		if a.budget.left.Add(-n) < 0 {
			return nil, fmt.Errorf("%w: document expands past %d bytes, far more than real documents of its size", core.ErrTooLarge, a.budget.total)
		}
		if int64(buf.Len()) > a.lim.maxEntry {
			return nil, fmt.Errorf("%w: ZIP entry %q expands past %d bytes", core.ErrTooLarge, f.Name, a.lim.maxEntry)
		}
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, entryError(f, err)
		}
	}
}

// collectAfter is the part size worth a collection of its own. The collector waits
// until the heap has doubled since it last ran, and a large part has just pushed that
// target up, so without this the next one lands on top of this one's garbage, and so
// does the engine's verification pass. Against the cost of a part this size, a
// collection is cheap.
const collectAfter = 16 << 20

// release is called once a part has been written and nothing refers to its bytes.
func release(f *zip.File) {
	if f.UncompressedSize64 >= collectAfter {
		runtime.GC()
	}
}

func entryError(f *zip.File, err error) error {
	if errors.Is(err, zip.ErrAlgorithm) {
		return core.Unsupportedf("ZIP entry %q: %v", f.Name, err)
	}
	return core.Malformedf("ZIP entry %q: %v", f.Name, err)
}

var (
	fieldBeforeArchive = fieldSpec{"Data before the ZIP archive", core.CatHidden, core.RiskHigh, core.ActionRemove}
	fieldAfterArchive  = fieldSpec{"Data after the ZIP archive", core.CatHidden, core.RiskHigh, core.ActionRemove}
	fieldArchiveNote   = fieldSpec{"ZIP comment", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldEntryNote     = fieldSpec{"ZIP entry comment", core.CatHidden, core.RiskMed, core.ActionRemove}
	fieldEntryTimes    = fieldSpec{"ZIP entry times", core.CatTime, core.RiskMed, core.ActionRemove}
	fieldEntryExtra    = fieldSpec{"ZIP extra fields", core.CatHidden, core.RiskLow, core.ActionRemove}
)

// report describes what the rewrite discards from the archive layer. Everything
// here goes simply because it is never copied; the report is the user's only
// chance to learn that it was there.
func (a *archive) report(rep *core.Report) error {
	t := newTally("ZIP archive")
	before, err := a.bytesBefore()
	if err != nil {
		return err
	}
	if before > 0 {
		t.value(fieldBeforeArchive, byteCount(before))
	}
	after, err := a.bytesAfter()
	if err != nil {
		return err
	}
	if after > 0 {
		t.value(fieldAfterArchive, byteCount(after))
	}
	if a.zr.Comment != "" {
		t.value(fieldArchiveNote, a.zr.Comment)
	}
	var stamped, extras int
	var oldest, newest string
	for _, f := range a.entries {
		if f.Comment != "" {
			t.value(fieldEntryNote, f.Comment)
		}
		if hasMeaningfulExtra(f.Extra) {
			extras++
		}
		if f.ModifiedDate == 0 || f.ModifiedDate == msdosEpoch && f.ModifiedTime == 0 {
			continue
		}
		stamped++
		stamp := f.Modified.UTC().Format("2006-01-02 15:04:05")
		if oldest == "" || stamp < oldest {
			oldest = stamp
		}
		if stamp > newest {
			newest = stamp
		}
	}
	if stamped > 0 {
		t.value(fieldEntryTimes, fmt.Sprintf("%d entries, %s to %s", stamped, oldest, newest))
	}
	if extras > 0 {
		t.value(fieldEntryExtra, fmt.Sprintf("%d entries", extras))
	}
	t.flush(rep)
	return nil
}

// hasMeaningfulExtra reports whether an extra field holds anything other than
// ZIP64 sizes. A very large output needs ZIP64, which says nothing about anyone,
// and must not count as a leftover when the output is inspected.
func hasMeaningfulExtra(extra []byte) bool {
	for len(extra) >= 4 {
		id, size := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		if id != zip64ExtraID || size > len(extra)-4 {
			return true
		}
		extra = extra[4+size:]
	}
	return len(extra) > 0
}

// bytesBefore returns how much precedes the first local file header. The reader
// hides this offset, so it is recovered by finding the local header that ends
// exactly where the first entry's data begins.
func (a *archive) bytesBefore() (int64, error) {
	first := int64(-1)
	for _, f := range a.entries {
		off, err := f.DataOffset()
		if err != nil {
			return 0, entryError(f, err)
		}
		if first < 0 || off < first {
			first = off
		}
	}
	if first < 0 {
		return 0, nil // no entries, so nothing to measure from
	}
	const longestHeader = 30 + 2*65535
	window := min(first, longestHeader)
	buf := make([]byte, window)
	if _, err := a.src.ReadAt(buf, first-window); err != nil && !errors.Is(err, io.EOF) {
		return 0, core.Malformedf("ZIP archive: %v", err)
	}
	signature := []byte("PK\x03\x04")
	for end := len(buf); end > 0; {
		p := bytes.LastIndex(buf[:end], signature)
		if p < 0 {
			break
		}
		if p+30 <= len(buf) {
			nameLen := int(binary.LittleEndian.Uint16(buf[p+26:]))
			extraLen := int(binary.LittleEndian.Uint16(buf[p+28:]))
			if p+30+nameLen+extraLen == len(buf) {
				return first - window + int64(p), nil
			}
		}
		end = p + len(signature) - 1
	}
	return 0, core.Malformedf("ZIP archive: cannot find the first local file header")
}

// bytesAfter returns how much follows the end-of-central-directory record and
// its comment. It repeats the reader's search so that both agree on which record
// is the real one, since the reader tolerates trailing bytes but does not expose
// their length.
func (a *archive) bytesAfter() (int64, error) {
	end := a.endOfCentralDirectory()
	if end < 0 {
		return 0, core.Malformedf("ZIP archive: cannot find the end of the central directory")
	}
	return a.src.Size() - end, nil
}

// endOfCentralDirectory returns the offset just past the record and its comment,
// or -1 if no record is found.
func (a *archive) endOfCentralDirectory() int64 {
	const (
		searchWindow = 65 * 1024 // what archive/zip searches
		recordLen    = 22
	)
	size := a.src.Size()
	window := min(size, searchWindow)
	buf := make([]byte, window)
	if _, err := a.src.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
		return -1
	}
	for i := len(buf) - recordLen; i >= 0; i-- {
		if buf[i] != 'P' || !bytes.HasPrefix(buf[i:], []byte("PK\x05\x06")) {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(buf[i+recordLen-2:]))
		if i+recordLen+commentLen <= len(buf) {
			return size - window + int64(i+recordLen+commentLen)
		}
		return -1
	}
	return -1
}

// archiveWriter produces the new ZIP. A nil *archiveWriter discards everything,
// which is how Inspect runs the same code as Clean without compressing anything.
type archiveWriter struct {
	zw   *zip.Writer
	fl   *flate.Writer
	comp bytes.Buffer
}

func newArchiveWriter(dst io.Writer) (*archiveWriter, error) {
	if dst == nil {
		return nil, nil
	}
	fl, err := flate.NewWriter(io.Discard, flate.DefaultCompression)
	if err != nil {
		return nil, fmt.Errorf("deflate: %w", err)
	}
	return &archiveWriter{zw: zip.NewWriter(dst), fl: fl}, nil
}

// add appends an entry. It uses CreateRaw rather than CreateHeader for two
// reasons: CreateHeader always sets the data-descriptor flag, which leaves the
// sizes out of the local header, and ODF readers need the stored mimetype entry's
// real size there; and it is the only path that writes no extra field for a set
// time. The deprecated MS-DOS fields are therefore set directly, because setting
// FileHeader.Modified would add an extended-timestamp extra field.
func (w *archiveWriter) add(ctx context.Context, name string, data []byte, stored bool) error {
	if w == nil {
		return nil
	}
	fh := &zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CreatorVersion:     20,
		ReaderVersion:      20,
		ModifiedDate:       msdosEpoch,
		CRC32:              crc32.ChecksumIEEE(data),
		UncompressedSize64: uint64(len(data)),
		CompressedSize64:   uint64(len(data)),
	}
	if needsUTF8Flag(name) {
		fh.Flags |= 0x800
	}
	body := data
	if !stored && len(data) > 0 {
		compressed, err := w.deflate(ctx, data)
		if err != nil {
			return err
		}
		fh.Method = zip.Deflate
		fh.CompressedSize64 = uint64(len(compressed))
		body = compressed
	}
	entry, err := w.zw.CreateRaw(fh)
	if err != nil {
		return fmt.Errorf("write ZIP entry %q: %w", name, err)
	}
	if _, err := entry.Write(body); err != nil {
		return fmt.Errorf("write ZIP entry %q: %w", name, err)
	}
	return nil
}

func (w *archiveWriter) deflate(ctx context.Context, data []byte) ([]byte, error) {
	w.comp.Reset()
	w.fl.Reset(&w.comp)
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(len(data), readChunk)
		if _, err := w.fl.Write(data[:n]); err != nil {
			return nil, fmt.Errorf("deflate: %w", err)
		}
		data = data[n:]
	}
	if err := w.fl.Close(); err != nil {
		return nil, fmt.Errorf("deflate: %w", err)
	}
	return w.comp.Bytes(), nil
}

func (w *archiveWriter) close() error {
	if w == nil {
		return nil
	}
	if err := w.zw.Close(); err != nil {
		return fmt.Errorf("write ZIP directory: %w", err)
	}
	return nil
}

// needsUTF8Flag is true for names beyond ASCII that are valid UTF-8. Anything
// else keeps the flag clear, which leaves a legacy code-page name exactly as it
// was found.
func needsUTF8Flag(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	return strings.ContainsFunc(name, func(r rune) bool { return r >= utf8.RuneSelf })
}

func byteCount(n int64) string {
	return fmt.Sprintf("%d bytes", n)
}
