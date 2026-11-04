package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

// TestResidueFoundAcrossEveryBoundary places each signature so that it straddles the
// read boundary at every possible split, with a buffer barely longer than the longest
// signature, which is where an overlap off by one would show.
func TestResidueFoundAcrossEveryBoundary(t *testing.T) {
	longest := 0
	for _, s := range residueSignatures {
		longest = max(longest, len(s.bytes))
	}
	for _, size := range []int{longest + 1, longest + 2, 2 * longest, 64} {
		for _, s := range residueSignatures {
			for prefix := range 3 * size {
				data := append(bytes.Repeat([]byte{'.'}, prefix), s.bytes...)
				data = append(data, "tail"...)
				hits, err := scanResidueBuffer(context.Background(), bytes.NewReader(data), make([]byte, size))
				if err != nil {
					t.Fatal(err)
				}
				if !hasHit(hits, s.name, int64(prefix)) {
					t.Fatalf("buffer %d, prefix %d: %q not found, hits %v", size, prefix, s.name, hits)
				}
			}
		}
	}
}

func hasHit(hits []residueHit, name string, offset int64) bool {
	for _, h := range hits {
		if h.name == name && h.offset == offset {
			return true
		}
	}
	return false
}

// TestResidueSurvivesShortReads uses readers that hand back as little as they may.
func TestResidueSurvivesShortReads(t *testing.T) {
	data := []byte("some bytes then <x:xmpmeta and more then ns.adobe.com/xap at the end")
	for name, r := range map[string]*bytes.Reader{"one": bytes.NewReader(data), "half": bytes.NewReader(data)} {
		reader := iotest.OneByteReader(r)
		if name == "half" {
			reader = iotest.HalfReader(r)
		}
		hits, err := scanResidue(context.Background(), reader)
		if err != nil {
			t.Fatal(err)
		}
		if !hasHit(hits, "<x:xmpmeta", 16) || !hasHit(hits, "ns.adobe.com/xap", 41) {
			t.Errorf("%s: hits %v", name, hits)
		}
	}
}

func TestResidueRealBufferBoundary(t *testing.T) {
	sig := []byte("<?xpacket")
	for split := 1; split < len(sig); split++ {
		data := make([]byte, residueBufferSize-split)
		data = append(data, sig...)
		hits, err := scanResidue(context.Background(), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if !hasHit(hits, "<?xpacket", int64(residueBufferSize-split)) {
			t.Errorf("split %d: hits %v", split, hits)
		}
	}
}

func TestResidueReportsEachSignatureOnce(t *testing.T) {
	data := strings.Repeat("Photoshop 3.0 ", 100)
	hits, err := scanResidue(context.Background(), strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].offset != 0 {
		t.Errorf("hits: %v", hits)
	}
}

func TestResidueCleanDataIsQuiet(t *testing.T) {
	data := []byte("<x:xmpmet Photoshop 3.1 ns.adobe.com/xa <?xpacke")
	hits, err := scanResidue(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("false positive: %v", hits)
	}
	hits, err = scanResidue(context.Background(), bytes.NewReader(nil))
	if err != nil || len(hits) != 0 {
		t.Errorf("empty input: %v %v", hits, err)
	}
}

func TestResidueHonoursCancellationAndReadErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanResidue(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	broken := errors.New("disk gone")
	if _, err := scanResidue(context.Background(), iotest.ErrReader(broken)); !errors.Is(err, broken) {
		t.Errorf("read error: %v", err)
	}
}

func TestZoneFindings(t *testing.T) {
	stream := "[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=https://example.invalid/page\r\nHostUrl=https://example.invalid/file.jpg\r\nHostIpAddress=192.0.2.1\r\n"
	check := func(t *testing.T, data []byte) {
		t.Helper()
		found := make(map[string]string)
		for _, f := range zoneFindings(data, "here") {
			found[f.Field] = f.Value
			if f.Category != "filesystem" || f.Action != "remove" || f.Where != "here" {
				t.Errorf("finding: %+v", f)
			}
		}
		if found["Download URL"] != "https://example.invalid/file.jpg" {
			t.Errorf("URL: %+v", found)
		}
		if found["Download referrer URL"] != "https://example.invalid/page" {
			t.Errorf("referrer: %+v", found)
		}
		if found["Download zone"] != "3 (internet)" {
			t.Errorf("zone: %+v", found)
		}
		if !strings.Contains(found["Download mark field"], "192.0.2.1") {
			t.Errorf("other field: %+v", found)
		}
	}
	t.Run("ANSI", func(t *testing.T) { check(t, []byte(stream)) })
	t.Run("UTF-16LE", func(t *testing.T) {
		data := []byte{0xFF, 0xFE}
		data = append(data, utf16Bytes(stream, 0)...)
		check(t, data)
	})
	t.Run("UTF-16BE", func(t *testing.T) {
		data := []byte{0xFE, 0xFF}
		data = append(data, utf16Bytes(stream, 1)...)
		check(t, data)
	})
	t.Run("empty stream still counts", func(t *testing.T) {
		found := zoneFindings(nil, "here")
		if len(found) != 1 || found[0].Field != "Download mark" {
			t.Errorf("findings: %+v", found)
		}
	})
	t.Run("hostile bytes do not panic", func(t *testing.T) {
		zoneFindings([]byte{0xFF, 0xFE, 0x41}, "here")
		zoneFindings([]byte("=\n=\n===\n\x00\x1b"), "here")
	})
}
