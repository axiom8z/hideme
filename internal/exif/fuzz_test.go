package exif

import (
	"encoding/binary"
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
)

func FuzzParse(f *testing.F) {
	for _, order := range bothOrders {
		f.Add(coretest.FakeIFD(6).Encode(order))
		f.Add(minimalInOrder(order, 6))
		f.Add(ifdChain(order, 3))
		f.Add(appendIFD(rawHeader(order, headerSize), order, headerSize))
		f.Add(appendIFD(rawHeader(order, headerSize), order, 0, pointer(tagExifIFD, headerSize)))
		f.Add(appendIFD(rawHeader(order, headerSize), order, 0, rawEntry{tag: 0x927C, typ: typeDouble, count: 0xFFFFFFFF, value: 0xFFFFFFF0}))
	}
	for o := uint16(1); o <= 8; o++ {
		f.Add(Minimal(o))
	}
	f.Add(append([]byte(Prefix), Minimal(6)...))
	f.Add(coretest.FakeEXIF(0))
	f.Add(rawHeader(binary.BigEndian, 0xFFFFFFFF))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, block []byte) {
		checkBlock(t, block)
		checkBlock(t, StripPrefix(block))
	})
}
