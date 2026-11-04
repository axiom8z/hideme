package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/engine"
	"github.com/axiom8z/hideme/internal/formats/all/samples"
)

// stackTIFF has more bytes that no tag refers to than bytes of image, as a scientific
// stack that keeps its later frames outside the directory chain does.
func stackTIFF() []byte {
	return append(bytes.Clone(samples.TIFF().Data), bytes.Repeat([]byte{0xAB}, 4096)...)
}

func TestTIFFThatMayLoseFramesIsNotReplacedWithoutForce(t *testing.T) {
	data := stackTIFF()
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "stack.tif"), data)

	t.Run("in place is refused", func(t *testing.T) {
		out := hideme(t, "clean", "--in-place", src)
		if out.code != exitFailed {
			t.Errorf("exit %d", out.code)
		}
		for _, want := range []string{"FAILED (may-lose-content)", "may lack content", "original is untouched", "--force", "-o DIR"} {
			if !strings.Contains(out.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out.stdout)
			}
		}
		if !bytes.Equal(get(t, src), data) {
			t.Fatal("the original was replaced")
		}
		doc := parseJSON(t, hideme(t, "clean", "--json", "--in-place", src).stdout)
		if r := doc.Results[0]; r.Status != engine.StatusFailed || r.ErrorKind != "may-lose-content" || !r.MayLoseContent {
			t.Errorf("result: %+v", r)
		}
	})
	t.Run("a dry run says the same", func(t *testing.T) {
		out := hideme(t, "clean", "--in-place", "--dry-run", src)
		if out.code != exitFailed || !strings.Contains(out.stdout, "a real run would fail") {
			t.Errorf("exit %d:\n%s", out.code, out.stdout)
		}
	})
	t.Run("a separate output is written, with the warning", func(t *testing.T) {
		outDir := filepath.Join(dir, "out")
		out := hideme(t, "clean", "-o", outDir, src)
		if out.code != exitOK {
			t.Fatalf("exit %d\n%s", out.code, out.stdout)
		}
		if !exists(filepath.Join(outDir, "stack.tif")) {
			t.Error("no output")
		}
		if !strings.Contains(out.stdout, "WARNING") || !strings.Contains(out.stderr, "more frames") {
			t.Errorf("no warning:\n%s\n%s", out.stdout, out.stderr)
		}
		doc := parseJSON(t, hideme(t, "clean", "--json", "--force", "-o", outDir, src).stdout)
		if !doc.Results[0].MayLoseContent {
			t.Errorf("result: %+v", doc.Results[0])
		}
		if !bytes.Equal(get(t, src), data) {
			t.Error("the original changed")
		}
	})
	t.Run("inspect warns ahead of time", func(t *testing.T) {
		doc := parseJSON(t, hideme(t, "inspect", "--json", src).stdout)
		if !doc.Results[0].MayLoseContent {
			t.Errorf("result: %+v", doc.Results[0])
		}
	})
	t.Run("force replaces it", func(t *testing.T) {
		out := hideme(t, "clean", "--in-place", "--force", src)
		if out.code != exitOK {
			t.Fatalf("exit %d\n%s", out.code, out.stdout)
		}
		if bytes.Equal(get(t, src), data) {
			t.Error("--force did not replace the original")
		}
		if !strings.Contains(out.stdout, "--force replaced the original") {
			t.Errorf("no warning that the original is gone:\n%s", out.stdout)
		}
	})
	// An ordinary TIFF has nothing to fear from --in-place.
	t.Run("an ordinary TIFF is replaced as usual", func(t *testing.T) {
		plain := put(t, filepath.Join(dir, "plain.tif"), samples.TIFF().Data)
		if out := hideme(t, "clean", "--in-place", plain); out.code != exitOK {
			t.Errorf("exit %d\n%s", out.code, out.stdout)
		}
	})
}

func TestUsageDocumentsTheInPlaceRefusal(t *testing.T) {
	for _, want := range []string{"may lack content", "--force"} {
		if !strings.Contains(usageClean, want) {
			t.Errorf("clean usage lacks %q", want)
		}
	}
	for _, want := range []string{`"may_lose_content"`, `"may-lose-content"`} {
		if !strings.Contains(usageJSON, want) {
			t.Errorf("JSON usage lacks %q", want)
		}
	}
}

func TestResidueFailureIsExplainedInFull(t *testing.T) {
	dir := t.TempDir()
	content := []byte(liarMagic + "a manual about <x:xmpmeta and nothing else\n")
	src := put(t, filepath.Join(dir, "doc.liar"), content)
	wants := []string{"<x:xmpmeta", "original is untouched", "no output was written", "false alarm", "PDF about XMP"}

	text := hideme(t, "clean", src)
	asJSON := hideme(t, "clean", "--json", src)
	doc := parseJSON(t, asJSON.stdout)

	for _, want := range wants {
		if !strings.Contains(text.stdout, want) {
			t.Errorf("text lacks %q:\n%s", want, text.stdout)
		}
		if !strings.Contains(doc.Results[0].Reason, want) {
			t.Errorf("JSON lacks %q: %q", want, doc.Results[0].Reason)
		}
		if !strings.Contains(asJSON.stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, asJSON.stderr)
		}
	}
	if strings.Contains(doc.Results[0].Reason, "...") {
		t.Errorf("the explanation was cut off: %q", doc.Results[0].Reason)
	}
	if !bytes.Equal(get(t, src), content) || exists(filepath.Join(dir, "doc.clean.liar")) {
		t.Error("the file system does not match what the message says")
	}
}

// --json=true is the same request as --json, so a usage error must honour it as well.
func TestJSONDocumentHoweverTheFlagIsSpelled(t *testing.T) {
	for _, args := range [][]string{
		{"clean", "--json=true", "--nonsense", "x.png"},
		{"clean", "-json=1", "--nonsense", "x.png"},
		{"inspect", "--json=T", "--nonsense", "x.png"},
		{"formats", "--json=true", "--nonsense"},
		{"--json", "clean", "x.png"},
	} {
		out := hideme(t, args...)
		if out.code != exitUsage {
			t.Errorf("%q: exit %d", args, out.code)
		}
		if doc := parseJSON(t, out.stdout); doc.ExitCode != exitUsage || doc.Error == "" {
			t.Errorf("%q: document %+v", args, doc)
		}
	}
	if out := hideme(t, "clean", "--json", "--json=false", "--nonsense", "x.png"); out.stdout != "" {
		t.Errorf("the last spelling wins, as in the flag package: %q", out.stdout)
	}
}

// An empty -o is nearly always an unset variable in a script, and quietly ignoring it
// wrote beside the source or, with --in-place, replaced the original.
func TestEmptyOutputDirIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
	for _, args := range [][]string{
		{"clean", "-o", "", src},
		{"clean", "--in-place", "-o", "", src},
		{"clean", "-o=", src},
	} {
		out := hideme(t, args...)
		if out.code != exitUsage || !strings.Contains(out.stderr, "-o") {
			t.Errorf("%q: exit %d: %s", args, out.code, out.stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !bytes.Equal(get(t, src), samples.PNG().Data) {
		t.Errorf("a usage error did something: %v", entries)
	}
}

// Handlers quote what they found in a file when they refuse it, such as a ZIP entry
// name, and clean promises to keep what files hold out of its output.
func TestFailureReasonsDoNotRepeatFileText(t *testing.T) {
	r := engine.Result{Path: "a", Status: engine.StatusFailed, Reason: "malformed file", Detail: `entry "Test Author" is bad`}
	hidden := sanitize(r, display{detailHint: "ask for more"})
	if strings.Contains(hidden.Reason+hidden.Detail, "Test Author") || !strings.Contains(hidden.Reason, "malformed file") {
		t.Errorf("result: %+v", hidden)
	}
	if !strings.Contains(hidden.Reason, "ask for more") {
		t.Errorf("no way to find out more: %q", hidden.Reason)
	}
	if got := sanitize(r, display{showValues: true}); !strings.Contains(got.Reason, "Test Author") || got.Detail != "" {
		t.Errorf("-v must show it: %+v", got)
	}

	src := put(t, filepath.Join(t.TempDir(), "a.svg"),
		[]byte(`<?xml version="1.0" encoding="Test Author"?><svg xmlns="http://www.w3.org/2000/svg"/>`))
	for _, args := range [][]string{{"clean", src}, {"clean", "--json", src}, {"inspect", "--redact", src}} {
		out := hideme(t, args...)
		if strings.Contains(out.stdout+out.stderr, "Test Author") {
			t.Errorf("%q printed text from the file:\n%s\n%s", args, out.stdout, out.stderr)
		}
	}
}

// A dry run cleans nothing, so "cleaned: 0" misdescribes one where every file failed.
func TestFailingDryRunSummaryIsWordedAsADryRun(t *testing.T) {
	src := put(t, filepath.Join(t.TempDir(), "a.bin"), []byte("no format at all"))
	out := hideme(t, "clean", "--dry-run", src)
	if out.code != exitFailed {
		t.Fatalf("exit %d\n%s", out.code, out.stdout)
	}
	if strings.Contains(out.stdout, "cleaned: 0") || !strings.Contains(out.stdout, "would clean: 0") {
		t.Errorf("summary:\n%s", out.stdout)
	}
	if real := hideme(t, "clean", src); !strings.Contains(real.stdout, "cleaned: 0") || strings.Contains(real.stdout, "would clean") {
		t.Errorf("a real run's summary:\n%s", real.stdout)
	}
}

// These characters cannot be in a Windows file name, so every file would have failed
// one at a time after being cleaned. A wildcard in a suffix is a mistake on any system.
func TestSuffixWithWildcardIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
	for _, suffix := range []string{".a*", "*", ".a?", `.a"`, ".a<", ".a>", ".a|"} {
		out := hideme(t, "clean", "--suffix", suffix, src)
		if out.code != exitUsage || !strings.Contains(out.stderr, "suffix") {
			t.Errorf("%q: exit %d\n%s\n%s", suffix, out.code, out.stdout, out.stderr)
		}
		if doc := parseJSON(t, hideme(t, "clean", "--json", "--suffix", suffix, src).stdout); doc.ExitCode != exitUsage || len(doc.Results) != 0 {
			t.Errorf("%q: document %+v", suffix, doc)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("a usage error wrote something: %v %v", entries, err)
	}
}

// Every handler's errors, found by putting a marker at every position of its sample:
// text that a handler really does put in an error message never reaches the default
// output, by any route, however the handler wrote it. Whether it was quoted is beside the
// point now, so this also holds for a handler written next year.
func TestFailureOutputNeverHoldsFileText(t *testing.T) {
	const marker = "ZQXJ"
	digits := regexp.MustCompile(`\d+`)
	ctx := context.Background()
	found := 0
	for _, sample := range samples.All() {
		seen := make(map[string]bool)
		for pos := 0; pos+len(marker) <= len(sample.Data); pos++ {
			data := bytes.Clone(sample.Data)
			copy(data[pos:], marker)
			src := core.BytesSource(data)
			h, err := core.Detect(src)
			if err != nil {
				continue
			}
			_, err = h.Clean(ctx, src, io.Discard, core.Options{})
			if err == nil || !strings.Contains(err.Error(), marker) {
				continue
			}
			shape := digits.ReplaceAllString(err.Error(), "N")
			if seen[shape] {
				continue
			}
			seen[shape] = true
			found++
			path := put(t, filepath.Join(t.TempDir(), "x.bin"), data)
			for _, args := range [][]string{{"clean", path}, {"clean", "--json", path}, {"clean", "--dry-run", path}, {"inspect", "--redact", path}, {"inspect", "--redact", "--json", path}} {
				out := hideme(t, args...)
				if strings.Contains(out.stdout+out.stderr, marker) {
					t.Errorf("%s at %d: %q printed text from the file: %v\n%s\n%s", sample.Name, pos, args[:len(args)-1], err, out.stdout, out.stderr)
				}
			}
			if out := hideme(t, "clean", "-v", path); !strings.Contains(out.stdout, marker) {
				t.Errorf("%s at %d: -v must show the handler's words: %v\n%s", sample.Name, pos, err, out.stdout)
			}
			if out := hideme(t, "inspect", path); !strings.Contains(out.stdout, marker) {
				t.Errorf("%s at %d: inspect must show the handler's words: %v\n%s", sample.Name, pos, err, out.stdout)
			}
		}
	}
	// Without this, a change that stops handlers quoting would make the test pass on nothing.
	if found == 0 {
		t.Fatal("no handler quoted the marker, so this test has proved nothing")
	}
}
