package isobmff

import (
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
)

// FuzzClean runs both handlers on every input, because they share a parser and
// because the two must never disagree about who owns a file.
func FuzzClean(f *testing.F) {
	rich := plainMovie()
	rich.tracks = append(rich.tracks, gpsTrack())
	rich.tracks[0].refs = bx("cdsc", be32(3))
	rich.udta = richUserData()
	rich.moovMeta = phoneMeta()
	rich.moovFirst = true
	f.Add(rich.build())

	m4a := plainMovie()
	m4a.udta = iTunesUserData()
	m4a.after = []byte("SEFT")
	f.Add(m4a.build())

	co64 := plainMovie()
	co64.largeMdat = true
	co64.tracks[0].co64 = true
	co64.tracks[1].stz2Bits = 8
	f.Add(co64.build())

	f.Add(phonePhoto().build())

	grid := phonePhoto()
	grid.ilocVersion = 1
	grid.indexSize = 4
	grid.items[0] = itemSpec{id: 1, typ: "grid", data: []byte("GRID"), inIdat: true, props: []uint16{2, 5}}
	grid.items = append(grid.items, itemSpec{id: 7, typ: "hvc1", data: primaryData, hidden: true, props: []uint16{1}})
	grid.refs = append(grid.refs, itemRef{typ: "dimg", from: 1, to: []uint32{7}})
	f.Add(grid.build())

	large := phonePhoto()
	large.ilocVersion = 2
	large.offsetSize, large.lengthSize, large.baseSize, large.indexSize = 8, 8, 4, 8
	f.Add(large.build())

	f.Fuzz(func(t *testing.T, data []byte) {
		src := core.BytesSource(data)
		if heifHandler.Match(nil, src) && mp4Handler.Match(nil, src) {
			t.Fatal("both handlers matched")
		}
		coretest.FuzzTarget(t, heifHandler, data)
		coretest.FuzzTarget(t, mp4Handler, data)
	})
}
