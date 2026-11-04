package isobmff

import (
	"encoding/binary"
	"fmt"

	"github.com/axiom8z/hideme/internal/core"
)

// track is what a trak box says about one track.
type track struct {
	box      box
	id       uint32
	position int // 1-based place among the tracks, for reports: the ID is the file's
	handler  string
	format   string // the first sample entry's format
	samples  uint64
	bytes    uint64
	// sampleCount is the number of samples the size table declares.
	sampleCount uint64
}

// removed applies the track policy: the picture, sound and subtitles of a movie
// stay, and everything else goes, so that a telemetry format invented next year
// is removed too. pict and auxv are pictures: an image sequence is made of pict
// tracks, and auxv carries alpha or depth for a video track.
func (t *track) removed() bool {
	return !t.visual() && t.handler != "soun" && !t.subtitle()
}

// where locates the track in a report, which clean prints.
func (t *track) where() string { return fmt.Sprintf("moov/trak[%d]", t.position) }

func (t *track) visual() bool {
	return t.handler == "vide" || t.handler == "pict" || t.handler == "auxv"
}

func (t *track) subtitle() bool {
	switch t.handler {
	case "subt", "sbtl", "text", "clcp":
		return true
	}
	return false
}

// sampleTables holds the boxes that locate a track's samples.
type sampleTables struct {
	stsd, stsc, stsz, stz2, stco, co64 *box
}

// parseTrack reads a track's identity and sample locations without editing it.
// Tables that do not parse are refused for kept and removed tracks alike: the
// samples of a removed track are the GPS log itself, and without the kept tracks'
// ranges, unreferenced bytes cannot be told from real media.
func (a *analysis) parseTrack(m *image, trak box) (*track, error) {
	t := &track{box: trak}
	var tkhd, mdia, hdlr, minf, stbl, dinf *box
	if _, err := a.walk(m.data, trak.body, trak.end, func(b box) error {
		switch b.typ {
		case "tkhd":
			return assign(&tkhd, b)
		case "mdia":
			return assign(&mdia, b)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if mdia == nil {
		return nil, core.Malformedf("trak box without a mdia box")
	}
	if tkhd != nil {
		t.id = trackID(m.data[tkhd.body:tkhd.end])
	}
	if _, err := a.walk(m.data, mdia.body, mdia.end, func(b box) error {
		switch b.typ {
		case "hdlr":
			return assign(&hdlr, b)
		case "minf":
			return assign(&minf, b)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if hdlr == nil {
		return nil, core.Malformedf("mdia box without an hdlr box")
	}
	handler, err := handlerType(m.data[hdlr.body:hdlr.end])
	if err != nil {
		return nil, err
	}
	t.handler = handler
	if minf == nil {
		return t, nil
	}
	if _, err := a.walk(m.data, minf.body, minf.end, func(b box) error {
		switch b.typ {
		case "stbl":
			return assign(&stbl, b)
		case "dinf":
			return assign(&dinf, b)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if dinf != nil {
		if _, err := a.checkDataRefs(m, *dinf, "moov/trak/mdia/minf/dinf"); err != nil {
			return nil, err
		}
	}
	if stbl == nil {
		return t, nil
	}
	tables, err := a.findTables(m, *stbl)
	if err != nil {
		return nil, err
	}
	t.format = firstFormat(m.data, tables.stsd)
	return t, a.readChunks(m.data, t, tables)
}

// trackID reads the track ID from a tkhd payload, returning 0 for a box too short
// to hold one, which scrubTimes then refuses if the track is kept.
func trackID(p []byte) uint32 {
	at := 12 // after the version, flags and two 32-bit times
	if len(p) > 0 && p[0] == 1 {
		at = 20
	}
	if len(p) < at+4 {
		return 0
	}
	return binary.BigEndian.Uint32(p[at:])
}

func firstFormat(data []byte, stsd *box) string {
	const firstEntryType = 12 // after the version, flags, count and a size field
	if stsd == nil || stsd.end-stsd.body < firstEntryType+4 {
		return ""
	}
	return string(data[stsd.body+firstEntryType : stsd.body+firstEntryType+4])
}

func (a *analysis) findTables(m *image, stbl box) (*sampleTables, error) {
	tables := &sampleTables{}
	_, err := a.walk(m.data, stbl.body, stbl.end, func(b box) error {
		switch b.typ {
		case "stsd":
			return assign(&tables.stsd, b)
		case "stsc":
			return assign(&tables.stsc, b)
		case "stsz":
			return assign(&tables.stsz, b)
		case "stz2":
			return assign(&tables.stz2, b)
		case "stco":
			return assign(&tables.stco, b)
		case "co64":
			return assign(&tables.co64, b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if tables.stco != nil && tables.co64 != nil {
		return nil, core.Malformedf("stbl has both stco and co64")
	}
	if tables.stsz != nil && tables.stz2 != nil {
		return nil, core.Malformedf("stbl has both stsz and stz2")
	}
	return tables, nil
}

// readChunks computes the byte range of every chunk from the three tables.
//
// All counts are checked against the box holding the table before any arithmetic.
// Offsets and lengths are compared with the file size by subtraction, so a co64
// value near 2^64 cannot wrap around into something that looks valid. The
// multiplication cannot overflow, as both factors fit in 32 bits.
func (a *analysis) readChunks(data []byte, t *track, tab *sampleTables) error {
	offsets, err := parseChunkOffsets(data, tab)
	if err != nil {
		return err
	}
	if offsets.count == 0 {
		return nil
	}
	if tab.stsc == nil || (tab.stsz == nil && tab.stz2 == nil) {
		return core.Malformedf("track %d has chunks but no complete sample tables", t.id)
	}
	a.chunks += offsets.count
	if a.chunks > maxChunks {
		return fmt.Errorf("%w: more than %d chunks", core.ErrTooLarge, maxChunks)
	}
	runs, err := parseChunkRuns(data, *tab.stsc)
	if err != nil {
		return err
	}
	sizes, err := parseSampleSizes(data, tab)
	if err != nil {
		return err
	}
	t.sampleCount = sizes.count
	// See resolveOpenChunks for why these chunks are not given an exact length.
	open := t.handler == "soun" && sizes.table == nil

	fileSize := a.src.Size()
	var last span
	flush := func() {
		if last.end > last.start {
			if t.removed() {
				a.drop = append(a.drop, last)
			} else {
				a.keep = append(a.keep, last)
			}
		}
	}
	var sample uint64
	run := 0
	for c := range offsets.count {
		if c%contextStride == 0 {
			if err := a.ctx.Err(); err != nil {
				return err
			}
		}
		for run+1 < runs.count && runs.first(run+1) <= uint64(c)+1 {
			run++
		}
		perChunk := uint64(runs.samples(run))
		if perChunk == 0 {
			return core.Malformedf("track %d has a chunk of zero samples", t.id)
		}
		if !open {
			// A last chunk shorter than its run says is a common writer quirk.
			// Players stop at the size table's count, so what lies beyond it is
			// unreferenced, and is treated as such.
			perChunk = min(perChunk, sizes.count-sample)
		}
		off := offsets.at(c)
		if open {
			if off >= uint64(fileSize) {
				return core.Malformedf("track %d has a chunk at offset %d, past the end of the file", t.id, off)
			}
			a.open = append(a.open, openChunk{int64(off), int64(min(perChunk*uint64(sizes.constant), uint64(fileSize)))})
			t.samples += perChunk
			continue
		}
		n := sizes.chunkBytes(sample, perChunk)
		sample += perChunk
		t.samples += perChunk
		if n == 0 {
			continue
		}
		if off > uint64(fileSize) || n > uint64(fileSize)-off {
			return core.Malformedf("track %d: chunk at offset %d of %d bytes is outside the file", t.id, off, n)
		}
		if t.visual() && t.bytes == 0 {
			a.checkFirstFrame(off, n)
		}
		t.bytes += n
		start, end := int64(off), int64(off+n)
		if start == last.end {
			last.end = end
			continue
		}
		flush()
		last = span{start, end}
	}
	flush()
	return nil
}

// checkFirstFrame warns if a video track's first bytes begin a JPEG or PNG file.
func (a *analysis) checkFirstFrame(off, n uint64) {
	var head [4]byte
	if n < uint64(len(head)) || readFull(a.src, head[:], int64(off)) != nil {
		return
	}
	if string(head[:3]) == "\xFF\xD8\xFF" || string(head[:]) == "\x89PNG" {
		a.warnImageFrames()
	}
}

// chunkOffsets reads an stco or co64 table in place.
type chunkOffsets struct {
	table []byte
	count int
	width int
}

func (o chunkOffsets) at(i int) uint64 {
	if o.width == 4 {
		return uint64(binary.BigEndian.Uint32(o.table[i*4:]))
	}
	return binary.BigEndian.Uint64(o.table[i*8:])
}

func parseChunkOffsets(data []byte, tab *sampleTables) (chunkOffsets, error) {
	b, width := tab.stco, 4
	if b == nil {
		b, width = tab.co64, 8
	}
	if b == nil {
		return chunkOffsets{}, nil
	}
	p := data[b.body:b.end]
	if len(p) < 8 {
		return chunkOffsets{}, core.Malformedf("%s box of %d bytes is too short", b.typ, b.size())
	}
	table := p[8:]
	count := uint64(binary.BigEndian.Uint32(p[4:]))
	if count > uint64(len(table)/width) {
		return chunkOffsets{}, core.Malformedf("%s declares %d chunks but has room for %d", b.typ, count, len(table)/width)
	}
	return chunkOffsets{table: table, count: int(count), width: width}, nil
}

// chunkRuns reads an stsc table in place.
type chunkRuns struct {
	table []byte
	count int
}

func (r chunkRuns) first(i int) uint64 {
	return uint64(binary.BigEndian.Uint32(r.table[i*12:]))
}

func (r chunkRuns) samples(i int) uint32 {
	return binary.BigEndian.Uint32(r.table[i*12+4:])
}

func parseChunkRuns(data []byte, stsc box) (chunkRuns, error) {
	p := data[stsc.body:stsc.end]
	if len(p) < 8 {
		return chunkRuns{}, core.Malformedf("stsc box of %d bytes is too short", stsc.size())
	}
	table := p[8:]
	count := uint64(binary.BigEndian.Uint32(p[4:]))
	if count == 0 || count > uint64(len(table)/12) {
		return chunkRuns{}, core.Malformedf("stsc declares %d runs but has room for %d", count, len(table)/12)
	}
	runs := chunkRuns{table: table, count: int(count)}
	if runs.first(0) != 1 {
		return chunkRuns{}, core.Malformedf("stsc does not start at chunk 1")
	}
	for i := 1; i < runs.count; i++ {
		if runs.first(i) <= runs.first(i-1) {
			return chunkRuns{}, core.Malformedf("stsc chunk numbers are not increasing")
		}
	}
	return runs, nil
}

// sampleSizes reads an stsz or stz2 table in place. A nil table means every
// sample has the constant size, in which case the count is not limited by the
// box, since nothing has to be stored per sample.
type sampleSizes struct {
	constant uint32
	count    uint64
	table    []byte
	bits     int
}

func (s sampleSizes) at(i uint64) uint64 {
	switch s.bits {
	case 4:
		b := s.table[i/2]
		if i%2 == 0 {
			return uint64(b >> 4)
		}
		return uint64(b & 0x0F)
	case 8:
		return uint64(s.table[i])
	case 16:
		return uint64(binary.BigEndian.Uint16(s.table[i*2:]))
	}
	return uint64(binary.BigEndian.Uint32(s.table[i*4:]))
}

func (s sampleSizes) chunkBytes(first, n uint64) uint64 {
	if s.table == nil {
		return n * uint64(s.constant)
	}
	var total uint64
	for i := first; i < first+n; i++ {
		total += s.at(i)
	}
	return total
}

func parseSampleSizes(data []byte, tab *sampleTables) (sampleSizes, error) {
	if tab.stz2 != nil {
		return parseCompactSizes(data, *tab.stz2)
	}
	p := data[tab.stsz.body:tab.stsz.end]
	if len(p) < 12 {
		return sampleSizes{}, core.Malformedf("stsz box of %d bytes is too short", tab.stsz.size())
	}
	constant := binary.BigEndian.Uint32(p[4:])
	count := uint64(binary.BigEndian.Uint32(p[8:]))
	if constant != 0 {
		return sampleSizes{constant: constant, count: count}, nil
	}
	table := p[12:]
	if count > uint64(len(table)/4) {
		return sampleSizes{}, core.Malformedf("stsz declares %d samples but has room for %d", count, len(table)/4)
	}
	// A non-nil table marks the size as per sample even when there are no samples.
	return sampleSizes{count: count, table: table[: count*4 : count*4], bits: 32}, nil
}

func parseCompactSizes(data []byte, stz2 box) (sampleSizes, error) {
	p := data[stz2.body:stz2.end]
	if len(p) < 12 {
		return sampleSizes{}, core.Malformedf("stz2 box of %d bytes is too short", stz2.size())
	}
	bits := int(p[7])
	if bits != 4 && bits != 8 && bits != 16 {
		return sampleSizes{}, core.Malformedf("stz2 field size %d is not 4, 8 or 16", bits)
	}
	count := uint64(binary.BigEndian.Uint32(p[8:]))
	table := p[12:]
	if count > uint64(len(table))*8/uint64(bits) {
		return sampleSizes{}, core.Malformedf("stz2 declares %d samples but has room for %d", count, uint64(len(table))*8/uint64(bits))
	}
	return sampleSizes{count: count, table: table, bits: bits}, nil
}

// removeTrack frees a track's whole trak box and reports it. The samples were
// already queued for zeroing by readChunks.
func (a *analysis) removeTrack(m *image, t *track) {
	category, risk, field := core.CatHidden, core.RiskHigh, "Data track"
	switch {
	case t.handler == "tmcd":
		category, risk, field = core.CatTime, core.RiskMed, "Timecode track"
	case t.handler == "hint":
		category, risk, field = core.CatTechnical, core.RiskLow, "Hint track"
	case t.format == "gpmd":
		category, risk, field = core.CatLocation, core.RiskHigh, "GoPro telemetry track"
	case t.format == "camm":
		category, risk, field = core.CatLocation, core.RiskHigh, "Camera motion track"
	case t.handler == "meta":
		category, risk, field = core.CatLocation, core.RiskHigh, "Timed metadata track"
	}
	value := fmt.Sprintf("handler %q, format %q, %d samples, %d bytes", t.handler, t.format, t.samples, t.bytes)
	a.remove(category, risk, field, value, t.where())
	m.free(t.box)
}
