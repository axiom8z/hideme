package pdf

// object is one parsed PDF value: nil for null, bool, int64, pdfReal, pdfName,
// pdfString, pdfArray, *pdfDict, pdfRef or *pdfStream.
type object any

// pdfName is a name with its #xx escapes decoded.
type pdfName string

// pdfReal keeps a number's source text. Reformatting a real through float64
// could change it, which would alter the document for no benefit, and integers
// too large for int64 land here for the same reason.
type pdfReal string

// pdfString is a string's decoded bytes. Remembering which spelling it had keeps
// the rewrite close to the source and stable under a second pass.
type pdfString struct {
	data []byte
	hex  bool
}

type pdfArray []object

// pdfRef is an indirect reference, "num gen R".
type pdfRef struct{ num, gen int }

// pdfStream is a stream object. raw is the undecoded data, normally a slice of
// the input file, so a large file costs no second copy.
type pdfStream struct {
	dict *pdfDict
	raw  []byte
}

type dictEntry struct {
	key string
	val object
}

// pdfDict is a dictionary that remembers key order. The rewrite must be
// deterministic and idempotent, so nothing may depend on map iteration order.
type pdfDict struct {
	entries []dictEntry
	index   map[string]int // only built for large dictionaries
}

// dictIndexThreshold is where a linear key search stops being the cheaper
// option. Almost every real dictionary is below it; a hostile one with millions
// of keys is not, and must not cost quadratic time.
const dictIndexThreshold = 16

func (d *pdfDict) find(key string) int {
	if d.index != nil {
		if i, ok := d.index[key]; ok {
			return i
		}
		return -1
	}
	for i := range d.entries {
		if d.entries[i].key == key {
			return i
		}
	}
	return -1
}

// set adds a key or replaces its value in place. A file that repeats a key gets
// the last value, as qpdf and most viewers do, and the earlier value is gone.
func (d *pdfDict) set(key string, val object) {
	if i := d.find(key); i >= 0 {
		d.entries[i].val = val
		return
	}
	d.entries = append(d.entries, dictEntry{key, val})
	switch {
	case d.index != nil:
		d.index[key] = len(d.entries) - 1
	case len(d.entries) > dictIndexThreshold:
		d.index = make(map[string]int, 2*len(d.entries))
		for i := range d.entries {
			d.index[d.entries[i].key] = i
		}
	}
}

func (d *pdfDict) get(key string) object {
	if i := d.find(key); i >= 0 {
		return d.entries[i].val
	}
	return nil
}

func (d *pdfDict) has(key string) bool { return d.find(key) >= 0 }
