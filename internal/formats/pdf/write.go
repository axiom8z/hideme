package pdf

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
)

// headerVersion returns the version in the header. Nothing else on that line
// survives, and a version that cannot be read becomes 1.7, which the rewritten
// file's plain table and trailer satisfy like any other. The output never needs a
// higher version than its input because it adds no feature, and a higher /Version
// in the catalog takes precedence over the header anyway.
func headerVersion(data []byte) string {
	const fallback = "1.7"
	rest := data[len(pdfHeader):]
	major := digitRun(rest)
	if major < 1 || major > 2 || major >= len(rest) || rest[major] != '.' {
		return fallback
	}
	minor := digitRun(rest[major+1:])
	if minor < 1 || minor > 2 {
		return fallback
	}
	return string(rest[:major+1+minor])
}

func digitRun(b []byte) int {
	n := 0
	for n < len(b) && isDigit(b[n]) {
		n++
	}
	return n
}

// writeDocument lays the file out afresh: header, objects numbered from 1, one
// classic table, and a trailer holding only /Size and /Root. Object 1 is the
// catalog. There is deliberately no /ID, no /Info, and no producer of our own.
func writeDocument(ctx context.Context, dst io.Writer, version string, objects []object) error {
	w := bufio.NewWriterSize(dst, 1<<16)
	var pos int64
	write := func(b []byte) error {
		n, err := w.Write(b)
		pos += int64(n)
		if err != nil {
			return fmt.Errorf("write: %w", err)
		}
		return nil
	}
	// The second line is the customary marker that tells transfer tools this is
	// binary data.
	if err := write([]byte("%PDF-" + version + "\n%\xE2\xE3\xCF\xD3\n")); err != nil {
		return err
	}
	offsets := make([]int64, len(objects))
	var buf []byte
	for i, obj := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		offsets[i] = pos
		buf = strconv.AppendInt(buf[:0], int64(i+1), 10)
		buf = append(buf, " 0 obj\n"...)
		s, isStream := obj.(*pdfStream)
		if isStream {
			buf = appendObject(buf, s.dict)
			buf = append(buf, "\nstream\n"...)
			if err := write(buf); err != nil {
				return err
			}
			if err := write(s.raw); err != nil {
				return err
			}
			buf = append(buf[:0], "\nendstream"...)
		} else {
			buf = appendObject(buf, obj)
		}
		buf = append(buf, "\nendobj\n"...)
		if err := write(buf); err != nil {
			return err
		}
	}
	xrefAt := pos
	buf = fmt.Appendf(buf[:0], "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		buf = fmt.Appendf(buf, "%010d 00000 n \n", off)
		if len(buf) >= 1<<16 {
			if err := write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	buf = fmt.Appendf(buf, "trailer\n<</Size %d /Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefAt)
	if err := write(buf); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// appendObject serialises a value. Every byte of the output is decided here, so
// this is half of what makes cleaning idempotent: reading this text back must give
// the same value, which must then serialise to the same text.
func appendObject(b []byte, v object) []byte {
	switch t := v.(type) {
	case nil:
		return append(b, "null"...)
	case bool:
		return strconv.AppendBool(b, t)
	case int64:
		return strconv.AppendInt(b, t, 10)
	case pdfReal:
		return append(b, t...)
	case pdfName:
		return appendName(b, string(t))
	case pdfString:
		if t.hex {
			return appendHexString(b, t.data)
		}
		return appendLiteralString(b, t.data)
	case pdfRef:
		b = strconv.AppendInt(b, int64(t.num), 10)
		b = append(b, ' ')
		b = strconv.AppendInt(b, int64(t.gen), 10)
		return append(b, " R"...)
	case pdfArray:
		b = append(b, '[')
		for i, e := range t {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendObject(b, e)
		}
		return append(b, ']')
	case *pdfDict:
		b = append(b, "<<"...)
		for i, e := range t.entries {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendName(b, e.key)
			b = append(b, ' ')
			b = appendObject(b, e.val)
		}
		return append(b, ">>"...)
	}
	panic(fmt.Sprintf("pdf: cannot serialise a %T", v))
}

func appendName(b []byte, s string) []byte {
	b = append(b, '/')
	for i := range len(s) {
		c := s[i]
		if c < 0x21 || c > 0x7E || c == '#' || isDelimiter(c) {
			b = fmt.Appendf(b, "#%02X", c)
		} else {
			b = append(b, c)
		}
	}
	return b
}

func appendLiteralString(b, s []byte) []byte {
	b = append(b, '(')
	for _, c := range s {
		switch c {
		case '(', ')', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, c)
		}
	}
	return append(b, ')')
}

func appendHexString(b, s []byte) []byte {
	const digits = "0123456789ABCDEF"
	b = append(b, '<')
	for _, c := range s {
		b = append(b, digits[c>>4], digits[c&0x0F])
	}
	return append(b, '>')
}
