package pdf

import (
	"bytes"
	"testing"

	"github.com/axiom8z/hideme/internal/coretest"
)

func FuzzClean(f *testing.F) {
	f.Add(plainDoc())
	f.Add(infoDoc())
	f.Add(kitchenSink())
	f.Add(incrementalDoc())
	f.Add(xrefStreamDoc())
	f.Add(hybridDoc())
	f.Add(bytes.ReplaceAll(kitchenSink(), []byte("startxref"), []byte("startxxxx")))
	for _, c := range contractCases() {
		f.Add(c.Data)
	}
	f.Add([]byte("%PDF-"))
	f.Add([]byte("%PDF-1.7\n1 0 obj<<>>stream\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		coretest.FuzzTarget(t, testHandler, data)
	})
}
