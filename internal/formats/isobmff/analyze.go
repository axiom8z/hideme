package isobmff

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/axiom8z/hideme/internal/core"
)

// Limits on what a hostile file can make the analysis spend. Each failure is
// ErrTooLarge, so the file is refused rather than passed through. Real files sit
// far below these: a phone video's moov is a few megabytes with a few hundred
// boxes, and an hour of video has around a hundred thousand chunks.
const (
	maxLoadedBox     = 256 << 20 // a single moov, moof, mfra or HEIF meta
	maxRetained      = 512 << 20 // all edited boxes held until output
	maxTopLevelBoxes = 1 << 20
	maxNestedBoxes   = 1 << 22
	maxChunks        = 1 << 22
	maxFindings      = 4096
	// maxDecodeBytes bounds what is read from a removed box only to describe it.
	// The whole box is removed anyway, so a longer one only gets a vaguer report.
	maxDecodeBytes = 16 << 20
	// maxDecodeTotal bounds that across a whole file. Items can all point at the same
	// long extent, which would otherwise read it again for each one of them.
	maxDecodeTotal = 64 << 20
	contextStride  = 1 << 12 // how many loop steps pass between context checks
)

// analysis is one pass over a file. It plans every change as a patch and builds
// the report; applying the patches is left to Clean.
type analysis struct {
	ctx    context.Context
	src    core.Source
	opts   core.Options
	report *core.Report

	patches []core.Patch

	boxes    int             // nested boxes walked, against maxNestedBoxes
	chunks   int             // chunks read from sample tables, against maxChunks
	retained int64           // bytes of edited boxes held for output
	decoded  int64           // bytes read only to describe items, against maxDecodeTotal
	emitted  int             // findings offered to add, including repeats
	seen     map[string]bool // findings already reported
	overflow int             // findings over maxFindings

	warned map[string]bool // warnings already given

	mdats      []span // payloads of the top-level mdat boxes, in file order
	fragmented bool

	keep []span      // media bytes something still needs
	drop []span      // media bytes of removed tracks and items
	open []openChunk // kept chunks whose byte length the tables do not state
}

// openChunk is a legacy audio chunk: where it starts, and what its tables claim.
type openChunk struct{ start, claimed int64 }

// span is a half-open byte range.
type span struct{ start, end int64 }

// analyze plans the clean of src for a handler of the given kind.
func analyze(ctx context.Context, src core.Source, opts core.Options, want kind) (*analysis, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	det := detect(src)
	if det.kind != want || det.kind == kindNone {
		return nil, core.Unsupportedf("isobmff: not a file this handler owns")
	}
	if det.refuse != "" {
		return nil, core.Unsupportedf("isobmff: %s", det.refuse)
	}
	a := &analysis{
		ctx:    ctx,
		src:    src,
		opts:   opts,
		report: &core.Report{Format: det.format},
		seen:   make(map[string]bool),
		warned: make(map[string]bool),
	}
	if err := a.run(want); err != nil {
		return nil, err
	}
	if a.overflow > 0 {
		a.report.Add(core.Finding{
			Category: core.CatHidden,
			Risk:     core.RiskMed,
			Field:    "Further metadata items",
			Value:    fmt.Sprintf("%d more", a.overflow),
			Action:   core.ActionRemove,
		})
	}
	return a, nil
}

func (a *analysis) run(want kind) error {
	top, tail, err := a.topLevel()
	if err != nil {
		return err
	}
	a.dropTrailing(tail)
	var moov, meta, ftyp int
	for _, b := range top {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		switch b.typ {
		case "ftyp", "styp":
			if b.typ == "ftyp" {
				ftyp++
			}
			if b.end-b.body > maxBrandBox {
				return core.Malformedf("%s box of %d bytes is implausibly long", b.typ, b.size())
			}
			err = a.fileType(b)
		case "mdat":
			// A mdat's header does not move, so its payload is the only part any
			// sample offset may point into.
			a.mdats = append(a.mdats, span{b.body, b.end})
		case "free", "skip", "wide":
			err = a.scrubTop(b)
		case "moov":
			moov++
			err = a.movie(b)
		case "moof":
			err = a.fragment(b)
		case "mfra":
			err = a.fragmentIndex(b)
		case "sidx":
			err = a.segmentIndex(b)
		case "prft":
			err = a.producerTime(b)
		case "meta":
			if want == kindHEIF {
				meta++
				if meta == 1 {
					err = a.cleanHEIFMeta(b)
				}
				break
			}
			err = a.removeTop(b)
		default:
			err = a.removeTop(b)
		}
		if err != nil {
			return err
		}
	}
	// A second ftyp, moov or meta usually means files were concatenated. Readers
	// disagree about which one counts, so nothing can be promised about the rest.
	switch {
	case ftyp > 1:
		return core.Malformedf("%d ftyp boxes", ftyp)
	case moov > 1:
		return core.Malformedf("%d moov boxes", moov)
	case meta > 1:
		return core.Malformedf("%d top-level meta boxes", meta)
	case want == kindMP4 && moov == 0:
		return core.Malformedf("no moov box, so the media cannot be located")
	}
	if err := a.resolveMedia(); err != nil {
		return err
	}
	return a.sortPatches()
}

// warnOnce gives a warning unless the same text was already given, so that a file
// repeating itself cannot flood the report.
func (a *analysis) warnOnce(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if !a.warned[text] {
		a.warned[text] = true
		a.report.Warn("%s", text)
	}
}

// topLevel lists the top-level boxes and returns where they stop. It also sets
// fragmented, which has to be known before the moov is read.
func (a *analysis) topLevel() ([]box, int64, error) {
	var top []box
	tail, err := scanTop(a.src, func(b box) bool {
		top = append(top, b)
		if b.typ == "moof" {
			a.fragmented = true
		}
		return len(top) <= maxTopLevelBoxes
	})
	if err != nil {
		return nil, 0, err
	}
	if len(top) > maxTopLevelBoxes {
		return nil, 0, fmt.Errorf("%w: more than %d top-level boxes", core.ErrTooLarge, maxTopLevelBoxes)
	}
	return top, tail, nil
}

// dropTrailing removes whatever follows the last box. Phones append vendor
// trailers there, such as Samsung's SEFT block, and a player never reads them.
func (a *analysis) dropTrailing(tail int64) {
	size := a.src.Size()
	if tail >= size {
		return
	}
	a.remove(core.CatHidden, core.RiskHigh, "Trailing data", fmt.Sprintf("%d bytes after the last box", size-tail), "end of file")
	a.patch(core.Patch{Off: tail, Len: size - tail})
}

func (a *analysis) patch(p core.Patch) { a.patches = append(a.patches, p) }

// sortPatches orders the plan for core.CopyPatched. An overlap would only make
// Clean fail after Inspect had succeeded, so it is caught here, where both see it.
func (a *analysis) sortPatches() error {
	slices.SortFunc(a.patches, func(x, y core.Patch) int { return cmp.Compare(x.Off, y.Off) })
	var pos int64
	for _, p := range a.patches {
		if p.Off < pos {
			return core.Malformedf("regions to rewrite overlap at offset %d", p.Off)
		}
		pos = p.Off + p.Len
	}
	return nil
}

// add reports a finding once. A file cannot flood the report: repeated findings
// collapse, and beyond maxFindings they are only counted.
func (a *analysis) add(f core.Finding) {
	a.emitted++
	f.Value = core.SafeText(f.Value)
	key := f.Field + "\x00" + f.Value + "\x00" + string(f.Action)
	if a.seen[key] {
		return
	}
	if len(a.seen) >= maxFindings {
		a.overflow++
		return
	}
	a.seen[key] = true
	a.report.Add(f)
}

func (a *analysis) remove(category core.Category, risk core.Risk, field, value, where string) {
	a.add(core.Finding{Category: category, Risk: risk, Field: field, Value: value, Where: where, Action: core.ActionRemove})
}

func (a *analysis) retain(category core.Category, risk core.Risk, field, value, where, reason string) {
	a.add(core.Finding{Category: category, Risk: risk, Field: field, Value: value, Where: where, Action: core.ActionRetain, Reason: reason})
}

// load reads a top-level box into memory.
func (a *analysis) load(b box) (*image, error) {
	if b.size() > maxLoadedBox {
		return nil, fmt.Errorf("%w: %s box of %d bytes", core.ErrTooLarge, b.typ, b.size())
	}
	data := make([]byte, b.size())
	if err := readFull(a.src, data, b.start); err != nil {
		return nil, err
	}
	return &image{data: data, off: b.start}, nil
}

// commit plans the output of an edited image, if it changed.
func (a *analysis) commit(m *image) error {
	if !m.dirty {
		return nil
	}
	a.retained += int64(len(m.data))
	if a.retained > maxRetained {
		return fmt.Errorf("%w: more than %d bytes of edited boxes", core.ErrTooLarge, maxRetained)
	}
	a.patch(core.Patch{Off: m.off, Len: int64(len(m.data)), Data: m.data})
	return nil
}

// walk calls visit for each child box in data[start:end] and returns where the
// children stop. Fewer than 8 bytes may be left over, or a zero size field, which
// is how QuickTime terminates some containers. A child that overruns its parent
// is refused, because the parser has lost its place.
func (a *analysis) walk(data []byte, start, end int64, visit func(box) error) (int64, error) {
	pos := start
	for end-pos >= 8 {
		if binary.BigEndian.Uint32(data[pos:]) == 0 {
			break
		}
		b, ok := parseHeader(data[pos:end], pos, end-pos)
		if !ok {
			return 0, core.Malformedf("box at offset %d overruns its parent", pos)
		}
		a.boxes++
		if a.boxes > maxNestedBoxes {
			return 0, fmt.Errorf("%w: more than %d nested boxes", core.ErrTooLarge, maxNestedBoxes)
		}
		if a.boxes%contextStride == 0 {
			if err := a.ctx.Err(); err != nil {
				return 0, err
			}
		}
		if err := visit(b); err != nil {
			return 0, err
		}
		pos = b.end
	}
	return pos, nil
}

// editor describes what to do with the children of one container: types in keep
// stay as they are, types in edit go to a function, free space is zeroed, and
// everything else is freed and reported. That makes the container an allowlist,
// so a metadata box invented next year is removed too.
type editor struct {
	keep map[string]bool
	edit map[string]func(box) error
}

func (a *analysis) editChildren(m *image, start, end int64, where string, ed editor) error {
	tail, err := a.walk(m.data, start, end, func(b box) error {
		if fn, ok := ed.edit[b.typ]; ok {
			return fn(b)
		}
		switch {
		case ed.keep[b.typ]:
			if children, ok := keptChildren[b.typ]; ok {
				if b.typ == "clip" || b.typ == "matt" {
					a.warnOnce("this file has an old QuickTime clipping region or matte, which was kept because it changes how the video looks. Nothing checks what it holds")
				}
				return a.editChildren(m, b.body, b.end, where+"/"+b.typ, editor{keep: children})
			}
			return a.trimKept(m, b, where)
		case isFreeSpace(b.typ):
			a.scrubNested(m, b, where)
			return nil
		}
		a.removeNested(m, b, where)
		return nil
	})
	if err != nil {
		return err
	}
	a.scrubTail(m, tail, end, where)
	return nil
}

func isFreeSpace(typ string) bool { return typ == "free" || typ == "skip" || typ == "wide" }

func typeSet(types ...string) map[string]bool {
	set := make(map[string]bool, len(types))
	for _, t := range types {
		set[t] = true
	}
	return set
}

// reportFreeSpace reports what zeroing a free box removed. Editors leave stale
// metadata in free space, so non-zero bytes there are worth telling the user about.
func (a *analysis) reportFreeSpace(n int64, where string) {
	a.remove(core.CatHidden, core.RiskMed, "Data in free space", fmt.Sprintf("%d bytes", n), where)
}

func (a *analysis) scrubNested(m *image, b box, where string) {
	if m.zero(b.body, b.end) {
		a.reportFreeSpace(b.end-b.body, where)
	}
}

func (a *analysis) scrubTail(m *image, start, end int64, where string) {
	if start < end && m.zero(start, end) {
		a.reportFreeSpace(end-start, where)
	}
}

// trimKept zeroes what follows the last field of a box that stays, and its reserved
// fields. A table with a count that overruns its box is refused.
func (a *analysis) trimKept(m *image, b box, where string) error {
	n := keptLength(b.typ, m.data[b.body:b.end])
	if n > b.end-b.body && countedTables[b.typ] {
		return core.Malformedf("%s box of %d bytes declares more than it holds", b.typ, b.size())
	}
	if n >= 0 && n < b.end-b.body && m.zero(b.body+n, b.end) {
		a.reportPastEnd(b, b.end-b.body-n, where)
	}
	a.zeroReserved(m, b, where)
	return nil
}

func (a *analysis) reportPastEnd(b box, n int64, where string) {
	a.remove(core.CatHidden, core.RiskMed, "Data past the end of a box", fmt.Sprintf("%q, %d bytes", b.typ, n), where)
}

func (a *analysis) removeNested(m *image, b box, where string) {
	a.describeRemoved(b.typ, m.data[b.body:b.end], b.end-b.body, where)
	m.free(b)
}

// scrubTop zeroes a top-level free box in place, streaming because one can be
// any size at all.
func (a *analysis) scrubTop(b box) error {
	used, err := a.hasData(span{b.body, b.end})
	if err != nil || !used {
		return err
	}
	a.reportFreeSpace(b.end-b.body, b.typ)
	a.patch(core.Patch{Off: b.body, Len: b.end - b.body, Zero: true})
	return nil
}

// removeTop frees a top-level box that is not on the allowlist. Only the start is
// read, because the report does not need the rest and the box can be any size.
func (a *analysis) removeTop(b box) error {
	head := make([]byte, min(b.end-b.body, maxDecodeBytes))
	if err := readFull(a.src, head, b.body); err != nil {
		return err
	}
	a.describeRemoved(b.typ, head, b.end-b.body, "top level")
	a.patch(core.Patch{Off: b.start + 4, Len: 4, Data: []byte("free")})
	if b.body < b.end {
		a.patch(core.Patch{Off: b.body, Len: b.end - b.body, Zero: true})
	}
	return nil
}

// producerTime handles a prft box. The box is on the top-level allowlist because
// live streams use it to measure latency, but its NTP timestamp is the wall-clock
// time of capture, so that field is zeroed.
func (a *analysis) producerTime(b box) error {
	const ntpOffset, ntpSize = 8, 8 // after the version, flags and track ID
	if b.end-b.body < ntpOffset+ntpSize {
		return core.Malformedf("prft box of %d bytes is too short", b.size())
	}
	var head [ntpOffset + ntpSize]byte
	if err := readFull(a.src, head[:], b.body); err != nil {
		return err
	}
	if ntp := head[ntpOffset:]; !core.AllZero(ntp) {
		a.remove(core.CatTime, core.RiskMed, "Producer reference time", "wall-clock time of capture", "prft")
		a.patch(core.Patch{Off: b.body + ntpOffset, Len: ntpSize, Zero: true})
	}
	length := int64(20) // then a 32-bit media time, or a 64-bit one in version 1
	if head[0] == 1 {
		length = 24
	}
	return a.trimTop(b, length)
}

// segmentIndex handles a sidx box. It holds byte ranges and durations and stays,
// since players can use it to seek, but only as far as its references go.
func (a *analysis) segmentIndex(b box) error {
	var head [32]byte
	n := min(b.end-b.body, int64(len(head)))
	if err := readFull(a.src, head[:n], b.body); err != nil {
		return err
	}
	fixed := int64(24) // up to and including the count of references
	if head[0] == 1 {
		fixed = 32 // the first time and offset fields are 64-bit
	}
	if n < fixed {
		return core.Malformedf("sidx box of %d bytes is too short", b.size())
	}
	references := int64(binary.BigEndian.Uint16(head[fixed-2:]))
	return a.trimTop(b, fixed+references*12)
}

// trimTop zeroes what follows the first length bytes of a top-level box's payload.
func (a *analysis) trimTop(b box, length int64) error {
	if length >= b.end-b.body {
		return nil
	}
	tail := span{b.body + length, b.end}
	used, err := a.hasData(tail)
	if err != nil || !used {
		return err
	}
	a.reportPastEnd(b, tail.end-tail.start, "top level")
	a.patch(core.Patch{Off: tail.start, Len: tail.end - tail.start, Zero: true})
	return nil
}

// fragment edits a moof box. The media it describes stays untouched.
func (a *analysis) fragment(top box) error {
	m, err := a.load(top)
	if err != nil {
		return err
	}
	const where = "moof"
	traf := func(b box) error {
		return a.editChildren(m, b.body, b.end, where+"/traf", editor{
			keep: keepInTraf,
			edit: map[string]func(box) error{
				"sgpd": func(b box) error { return a.editSampleGroup(m, b, where+"/traf") },
				"sbgp": func(b box) error { return a.editSampleGroup(m, b, where+"/traf") },
			},
		})
	}
	err = a.editChildren(m, top.body-top.start, int64(len(m.data)), where, editor{
		keep: keepInFragment,
		edit: map[string]func(box) error{
			"traf": traf,
			"pssh": func(box) error { return fmt.Errorf("%w: protection data in a movie fragment", core.ErrEncrypted) },
		},
	})
	if err != nil {
		return err
	}
	return a.commit(m)
}

// fragmentIndex edits a mfra box, which holds only offsets to fragments.
func (a *analysis) fragmentIndex(top box) error {
	m, err := a.load(top)
	if err != nil {
		return err
	}
	err = a.editChildren(m, top.body-top.start, int64(len(m.data)), "mfra", editor{keep: keepInIndex})
	if err != nil {
		return err
	}
	return a.commit(m)
}

// hasData reports whether any byte in the file range is non-zero. It streams with
// a fixed buffer, so a hostile multi-gigabyte region costs time but not memory.
func (a *analysis) hasData(s span) (bool, error) {
	var buf []byte
	for pos := s.start; pos < s.end; {
		if err := a.ctx.Err(); err != nil {
			return false, err
		}
		n := min(s.end-pos, 1<<20)
		if buf == nil {
			buf = make([]byte, n)
		}
		chunk := buf[:min(n, int64(len(buf)))]
		if err := readFull(a.src, chunk, pos); err != nil {
			return false, err
		}
		if !core.AllZero(chunk) {
			return true, nil
		}
		pos += int64(len(chunk))
	}
	return false, nil
}

// mergeSpans sorts spans and joins any that overlap or touch.
func mergeSpans(spans []span) []span {
	sorted := slices.Clone(spans)
	slices.SortFunc(sorted, func(x, y span) int { return cmp.Compare(x.start, y.start) })
	out := sorted[:0]
	for _, s := range sorted {
		if n := len(out); n > 0 && s.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, s.end)
			continue
		}
		out = append(out, s)
	}
	return out
}

// spansOverlap reports whether two merged lists share a byte.
func spansOverlap(x, y []span) (span, bool) {
	for i, j := 0, 0; i < len(x) && j < len(y); {
		switch {
		case x[i].end <= y[j].start:
			i++
		case y[j].end <= x[i].start:
			j++
		default:
			return span{max(x[i].start, y[j].start), min(x[i].end, y[j].end)}, true
		}
	}
	return span{}, false
}

// containing returns the span in sorted, disjoint spans that holds pos.
func containing(spans []span, pos int64) (span, bool) {
	i, found := slices.BinarySearchFunc(spans, pos, func(s span, p int64) int {
		switch {
		case p < s.start:
			return 1
		case p >= s.end:
			return -1
		}
		return 0
	})
	if !found {
		return span{}, false
	}
	return spans[i], true
}

// resolveMedia turns what the moov and meta said about media bytes into patches.
//
// Bytes in a mdat that nothing kept refers to are zeroed, because that is where
// the actual GPS log of a removed track lives, along with orphaned tracks and
// earlier versions left by editors. Fragmented files are skipped because the
// fragments' own references are not resolved here.
func (a *analysis) resolveMedia() error {
	if a.fragmented && len(a.drop) > 0 {
		return core.Unsupportedf("isobmff: removing an item from a fragmented file")
	}
	for _, list := range [][]span{a.keep, a.drop} {
		for _, s := range list {
			if m, ok := containing(a.mdats, s.start); !ok || s.end > m.end {
				return core.Malformedf("media range %d to %d is not inside a single mdat box", s.start, s.end)
			}
		}
	}
	if err := a.resolveOpenChunks(); err != nil {
		return err
	}
	keep := mergeSpans(a.keep)
	drop := mergeSpans(a.drop)
	if s, overlap := spansOverlap(keep, drop); overlap {
		return core.Malformedf("bytes %d to %d belong to both a kept and a removed track or item", s.start, s.end)
	}
	for _, s := range drop {
		a.patch(core.Patch{Off: s.start, Len: s.end - s.start, Zero: true})
	}
	if a.fragmented {
		if len(a.mdats) > 0 {
			a.report.Warn("fragmented file: bytes in media data that no fragment refers to were not checked")
		}
		return nil
	}
	return a.zeroOrphans(mergeSpans(append(keep, drop...)))
}

// resolveOpenChunks finds the extent of chunks whose tables do not state one.
//
// A legacy QuickTime audio track with a constant sample size counts samples in
// audio frames, and its size field often holds a placeholder such as 1, not the
// real frame size. The bytes of such a chunk cannot be computed without knowing
// every codec, and getting them wrong would zero real audio. Each chunk is
// instead taken to run up to the next thing known to be in the mdat, which can
// only ever keep too much, and the report says so.
func (a *analysis) resolveOpenChunks() error {
	if len(a.open) == 0 {
		return nil
	}
	boundaries := make([]int64, 0, len(a.keep)+len(a.drop)+len(a.open))
	for _, list := range [][]span{a.keep, a.drop} {
		for _, s := range list {
			boundaries = append(boundaries, s.start)
		}
	}
	for _, chunk := range a.open {
		boundaries = append(boundaries, chunk.start)
	}
	slices.Sort(boundaries)
	boundaries = slices.Compact(boundaries)

	var unchecked int64
	for _, chunk := range a.open {
		m, ok := containing(a.mdats, chunk.start)
		if !ok {
			return core.Malformedf("audio chunk at offset %d is not inside a mdat box", chunk.start)
		}
		end := m.end
		i, found := slices.BinarySearch(boundaries, chunk.start)
		if found {
			i++
		}
		if i < len(boundaries) {
			end = min(end, boundaries[i])
		}
		a.keep = append(a.keep, span{chunk.start, end})
		// Chunk by chunk: one that claims too much must not offset one that holds
		// more than it claims.
		unchecked += max(end-chunk.start-chunk.claimed, 0)
	}
	if unchecked > 0 {
		a.report.Warn("legacy QuickTime audio tables do not state chunk sizes, so %d bytes next to those chunks were kept without being checked", unchecked)
	}
	return nil
}

// zeroOrphans zeroes the parts of each mdat not in covered, which is sorted and
// disjoint, and reports them when they held anything.
func (a *analysis) zeroOrphans(covered []span) error {
	var orphaned int64
	i := 0
	for _, m := range a.mdats {
		pos := m.start
		flush := func(end int64) error {
			if end <= pos {
				return nil
			}
			gap := span{pos, end}
			used, err := a.hasData(gap)
			if err != nil || !used {
				return err
			}
			orphaned += gap.end - gap.start
			a.patch(core.Patch{Off: gap.start, Len: gap.end - gap.start, Zero: true})
			return nil
		}
		for i < len(covered) && covered[i].start < m.end {
			if err := flush(covered[i].start); err != nil {
				return err
			}
			pos = covered[i].end
			i++
		}
		if err := flush(m.end); err != nil {
			return err
		}
	}
	if orphaned > 0 {
		a.remove(core.CatHidden, core.RiskHigh, "Unreferenced media data", fmt.Sprintf("%d bytes no track or item uses", orphaned), "mdat")
	}
	return nil
}
