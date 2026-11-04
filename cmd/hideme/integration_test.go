package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/axiom8z/hideme/internal/core"
	"github.com/axiom8z/hideme/internal/coretest"
	"github.com/axiom8z/hideme/internal/engine"
	"github.com/axiom8z/hideme/internal/formats/all/samples"
)

// liar is a handler that lies: it reports nothing, copies its input through, and so
// keeps whatever metadata the file holds. Registered only in this test binary, it
// proves that the real command line discards the output of a handler that is wrong.
type liar struct{}

const liarMagic = "HIDEME-TEST-LIAR\n"

func init() { core.Register(liar{}) }

func (liar) Format() core.Format {
	return core.Format{ID: "liar", Name: "Lying test format", Family: core.FamilyDocument, Extensions: []string{".liar"}}
}

func (liar) Match(head []byte, _ core.Source) bool {
	return bytes.HasPrefix(head, []byte(liarMagic))
}

func (liar) Inspect(context.Context, core.Source, core.Options) (*core.Report, error) {
	return &core.Report{Format: "liar"}, nil
}

func (liar) Clean(ctx context.Context, src core.Source, dst io.Writer, _ core.Options) (*core.Report, error) {
	if err := core.CopyRange(ctx, dst, src, 0, src.Size()); err != nil {
		return nil, err
	}
	return &core.Report{Format: "liar"}, nil
}

type outcome struct {
	code           int
	stdout, stderr string
}

func hideme(t *testing.T, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return outcome{code, stdout.String(), stderr.String()}
}

func put(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func get(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// putAllSamples writes one file per handler into dir and returns them by name.
func putAllSamples(t *testing.T, dir string) map[string]samples.Sample {
	t.Helper()
	all := make(map[string]samples.Sample)
	for _, s := range samples.All() {
		put(t, filepath.Join(dir, s.Name), s.Data)
		all[s.Name] = s
	}
	return all
}

func cleanedName(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + ".clean" + ext
}

func parseJSON(t *testing.T, stdout string) jsonDocument {
	t.Helper()
	var doc jsonDocument
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("text follows the JSON document:\n%s", stdout)
	}
	if doc.Schema != jsonSchema {
		t.Errorf("schema %q", doc.Schema)
	}
	return doc
}

// jsonDocument mirrors the documented schema, and parsing with DisallowUnknownFields
// makes a change to the schema fail here until the documentation has caught up.
type jsonDocument struct {
	Schema   string       `json:"schema"`
	Command  string       `json:"command"`
	DryRun   bool         `json:"dry_run"`
	Results  []jsonResult `json:"results"`
	Summary  jsonSummary  `json:"summary"`
	ExitCode int          `json:"exit_code"`
	Error    string       `json:"error"`
}

func TestCleanEveryFormatEndToEnd(t *testing.T) {
	dir := t.TempDir()
	all := putAllSamples(t, dir)
	paths := make([]string, 0, len(all))
	for name := range all {
		paths = append(paths, filepath.Join(dir, name))
	}
	slices.Sort(paths)

	out := hideme(t, append([]string{"clean"}, paths...)...)

	if out.code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", out.code, out.stdout, out.stderr)
	}
	for name, s := range all {
		cleaned := filepath.Join(dir, cleanedName(name))
		data := get(t, cleaned)
		for _, secret := range s.Secrets {
			if bytes.Contains(data, secret) {
				t.Errorf("%s still contains %q", name, secret)
			}
		}
		if s.Content != nil && !bytes.Contains(data, s.Content) {
			t.Errorf("%s lost its content", name)
		}
		if !bytes.Equal(get(t, filepath.Join(dir, name)), s.Data) {
			t.Errorf("the original %s changed", name)
		}
		// Clean what was cleaned, which must find nothing that needs removing.
		again := hideme(t, "inspect", "--json", cleaned)
		doc := parseJSON(t, again.stdout)
		for _, f := range doc.Results[0].Findings {
			if f.Action != core.ActionRetain {
				t.Errorf("%s: finding on the output: %+v", name, f)
			}
		}
	}
	if !strings.Contains(out.stdout, "Summary: files: 18, cleaned: 18, failed: 0") {
		t.Errorf("summary missing:\n%s", out.stdout)
	}
}

func TestCleanTextHidesValuesUnlessVerbose(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "photo.jpg"), samples.JPEG().Data)

	out := hideme(t, "clean", src)
	if out.code != exitOK {
		t.Fatalf("exit %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "GPS coordinates") {
		t.Errorf("field name missing:\n%s", out.stdout)
	}
	for _, value := range []string{coretest.EiffelTower, "Test Author", "Test Make"} {
		if strings.Contains(out.stdout, value) || strings.Contains(out.stderr, value) {
			t.Errorf("a removed value %q was printed:\n%s", value, out.stdout)
		}
	}

	verbose := hideme(t, "clean", "--force", "-v", src)
	if verbose.code != exitOK {
		t.Fatalf("exit %d: %s", verbose.code, verbose.stderr)
	}
	if !strings.Contains(verbose.stdout, coretest.EiffelTower) {
		t.Errorf("-v did not print the value:\n%s", verbose.stdout)
	}
}

func TestCleanJSONHidesValuesUnlessVerbose(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "photo.jpg"), samples.JPEG().Data)

	out := hideme(t, "clean", "--json", src)
	doc := parseJSON(t, out.stdout)
	if strings.Contains(out.stdout, coretest.EiffelTower) {
		t.Errorf("a removed value was printed:\n%s", out.stdout)
	}
	if doc.Command != "clean" || doc.ExitCode != exitOK || out.code != exitOK {
		t.Errorf("document: %+v, exit %d", doc, out.code)
	}
	r := doc.Results[0]
	if r.Status != "cleaned" || r.Format != "jpeg" || r.Output == "" || r.BytesAfter == 0 {
		t.Errorf("result: %+v", r)
	}
	if len(r.Findings) == 0 || r.Findings[0].Field == "" {
		t.Errorf("findings: %+v", r.Findings)
	}

	verbose := hideme(t, "clean", "--json", "--force", "-v", src)
	if !strings.Contains(verbose.stdout, coretest.EiffelTower) {
		t.Errorf("-v did not print the value")
	}
}

func TestInspectOutput(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "photo.jpg"), samples.JPEG().Data)

	out := hideme(t, "inspect", src)
	if out.code != exitOK {
		t.Fatalf("exit %d: %s", out.code, out.stderr)
	}
	high := strings.Index(out.stdout, "  high ")
	medium := strings.Index(out.stdout, "  medium ")
	low := strings.Index(out.stdout, "  low ")
	kept := strings.Index(out.stdout, "  kept ")
	if high < 0 || medium < 0 || low < 0 || kept < 0 || high > medium || medium > low || low > kept {
		t.Errorf("not grouped by risk then retained:\n%s", out.stdout)
	}
	line := findLine(out.stdout, "GPS coordinates")
	if !strings.Contains(line, coretest.EiffelTower) || !strings.Contains(line, "(at ") {
		t.Errorf("the GPS line has no value or location: %q", line)
	}
	if !strings.Contains(findLine(out.stdout, "Orientation"), "kept") {
		t.Errorf("orientation should be listed as kept:\n%s", out.stdout)
	}
	if !bytes.Equal(get(t, src), samples.JPEG().Data) {
		t.Error("inspect changed the file")
	}

	redacted := hideme(t, "inspect", "--redact", src)
	if strings.Contains(redacted.stdout, coretest.EiffelTower) || strings.Contains(redacted.stdout, "Test Author") {
		t.Errorf("--redact printed a value:\n%s", redacted.stdout)
	}
	if !strings.Contains(redacted.stdout, "GPS coordinates") {
		t.Errorf("--redact hid the field name:\n%s", redacted.stdout)
	}

	asJSON := hideme(t, "inspect", "--json", "--redact", src)
	doc := parseJSON(t, asJSON.stdout)
	if strings.Contains(asJSON.stdout, coretest.EiffelTower) {
		t.Errorf("--redact --json printed a value")
	}
	if doc.Results[0].Status != "inspected" {
		t.Errorf("result: %+v", doc.Results[0])
	}
}

func findLine(text, needle string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestNothingFoundIsReported(t *testing.T) {
	dir := t.TempDir()
	first := hideme(t, "clean", "-o", dir, put(t, filepath.Join(t.TempDir(), "photo.jpg"), samples.JPEG().Data), "--strict")
	if first.code != exitOK {
		t.Fatalf("exit %d: %s", first.code, first.stderr)
	}
	cleaned := filepath.Join(dir, "photo.jpg")

	out := hideme(t, "clean", "--strict", cleaned)
	if out.code != exitOK || !strings.Contains(out.stdout, "nothing found") {
		t.Errorf("exit %d:\n%s", out.code, out.stdout)
	}
	if !exists(filepath.Join(dir, "photo.clean.jpg")) {
		t.Error("a file with nothing to remove must still be rewritten")
	}
	doc := parseJSON(t, hideme(t, "inspect", "--json", "--strict", cleaned).stdout)
	if !doc.Results[0].NothingFound {
		t.Errorf("result: %+v", doc.Results[0])
	}
}

func TestFlagsAfterPathsAreHonoured(t *testing.T) {
	t.Run("in-place after the path", func(t *testing.T) {
		dir := t.TempDir()
		src := put(t, filepath.Join(dir, "photo.png"), samples.PNG().Data)

		out := hideme(t, "clean", src, "--in-place")

		if out.code != exitOK {
			t.Fatalf("exit %d: %s", out.code, out.stderr)
		}
		if bytes.Contains(get(t, src), []byte(samples.Author)) {
			t.Error("--in-place after a path was ignored, and the user believes it was replaced")
		}
		if exists(filepath.Join(dir, "photo.clean.png")) {
			t.Error("a copy was written as well")
		}
	})
	t.Run("flag with a value after the path", func(t *testing.T) {
		dir := t.TempDir()
		src := put(t, filepath.Join(dir, "a", "photo.png"), samples.PNG().Data)
		outDir := filepath.Join(dir, "out")

		out := hideme(t, "clean", src, "-o", outDir, "--timestamp", "zero")

		if out.code != exitOK {
			t.Fatalf("exit %d: %s", out.code, out.stderr)
		}
		if !exists(filepath.Join(outDir, "photo.png")) {
			t.Errorf("-o after a path was ignored:\n%s", out.stdout)
		}
	})
	t.Run("flags between paths", func(t *testing.T) {
		dir := t.TempDir()
		a := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
		b := put(t, filepath.Join(dir, "b.png"), samples.PNG().Data)

		out := hideme(t, "clean", a, "--suffix=.x", b)

		if out.code != exitOK {
			t.Fatalf("exit %d: %s", out.code, out.stderr)
		}
		if !exists(filepath.Join(dir, "a.x.png")) || !exists(filepath.Join(dir, "b.x.png")) {
			t.Errorf("wrong outputs:\n%s", out.stdout)
		}
	})
	t.Run("json after the path", func(t *testing.T) {
		dir := t.TempDir()
		src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
		parseJSON(t, hideme(t, "inspect", src, "--json").stdout)
	})
	t.Run("everything after -- is a path", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		put(t, "--in-place", samples.PNG().Data)

		out := hideme(t, "clean", "--", "--in-place")

		if out.code != exitOK {
			t.Fatalf("exit %d: %s\n%s", out.code, out.stderr, out.stdout)
		}
		if !bytes.Equal(get(t, "--in-place"), samples.PNG().Data) {
			t.Error("the file named --in-place was treated as the flag")
		}
		if !exists("--in-place.clean") {
			t.Errorf("no output:\n%s", out.stdout)
		}
	})
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "Usage"},
		{"unknown command", []string{"erase", src}, "unknown command"},
		{"unknown flag", []string{"clean", "--nonsense", src}, "nonsense"},
		{"unknown flag after a path", []string{"clean", src, "--nonsense"}, "nonsense"},
		{"clean flag on inspect", []string{"inspect", "--in-place", src}, "in-place"},
		{"no paths", []string{"clean"}, "no paths"},
		{"standard input", []string{"clean", "-"}, "standard input"},
		{"standard input after --", []string{"clean", "--", "-"}, "standard input"},
		{"flag missing its value", []string{"clean", src, "-o"}, "needs an argument"},
		{"jobs not a number", []string{"clean", "--jobs", "many", src}, "jobs"},
		{"negative jobs", []string{"clean", "--jobs", "-2", src}, "jobs"},
		{"bad timestamp", []string{"clean", "--timestamp", "yesterday", src}, "yesterday"},
		{"in-place and output dir", []string{"clean", "--in-place", "-o", dir, src}, "cannot be combined"},
		{"in-place and random name", []string{"clean", "--in-place", "--random-name", src}, "cannot be combined"},
		{"in-place and suffix", []string{"clean", src, "--in-place", "--suffix", ".x"}, "cannot be combined"},
		{"empty suffix beside the source", []string{"clean", "--suffix", "", src}, "suffix"},
		{"suffix with a path in it", []string{"clean", "--suffix", "../../x", src}, "suffix"},
		{"output dir is a file", []string{"clean", "-o", src, src}, "not a directory"},
		{"paths for formats", []string{"formats", src}, "takes no paths"},
		{"arguments for version", []string{"version", "x"}, "takes no arguments"},
		{"unknown help topic", []string{"help", "nonsense"}, "no help"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := hideme(t, c.args...)
			if out.code != exitUsage {
				t.Errorf("exit %d, want %d", out.code, exitUsage)
			}
			if !strings.Contains(out.stderr, c.want) {
				t.Errorf("stderr does not mention %q:\n%s", c.want, out.stderr)
			}
			if out.stdout != "" {
				t.Errorf("stdout should be empty:\n%s", out.stdout)
			}
		})
	}
	if !bytes.Equal(get(t, src), samples.PNG().Data) {
		t.Error("a usage error changed the file")
	}
}

func TestUsageErrorIsInTheJSONToo(t *testing.T) {
	out := hideme(t, "clean", "--json", "--nonsense", "x.png")

	if out.code != exitUsage {
		t.Errorf("exit %d", out.code)
	}
	doc := parseJSON(t, out.stdout)
	if !strings.Contains(doc.Error, "nonsense") || doc.ExitCode != exitUsage || doc.Command != "clean" {
		t.Errorf("document: %+v", doc)
	}
	if doc.Results == nil {
		t.Error(`"results" should be an empty array, not null`)
	}
	if !strings.Contains(out.stderr, "nonsense") {
		t.Errorf("no message for a person on stderr: %s", out.stderr)
	}

	config := hideme(t, "clean", "--json", "--in-place", "--random-name", "x.png")
	if config.code != exitUsage || !strings.Contains(parseJSON(t, config.stdout).Error, "cannot be combined") {
		t.Errorf("exit %d:\n%s", config.code, config.stdout)
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{
		{"help"}, {"--help"}, {"-h"}, {"help", "clean"}, {"help", "inspect"}, {"help", "formats"},
		{"help", "version"}, {"help", "json"}, {"clean", "--help"}, {"inspect", "-h"}, {"formats", "--help"}, {"version", "--help"},
	} {
		out := hideme(t, args...)
		if out.code != exitOK {
			t.Errorf("%q: exit %d", args, out.code)
		}
		if out.stdout == "" || out.stderr != "" {
			t.Errorf("%q: stdout %q, stderr %q", args, out.stdout, out.stderr)
		}
	}
	// Each command's usage must state the codes that scripts rely on.
	for _, topic := range []string{"clean", "inspect", "formats"} {
		if !strings.Contains(helpTopics[topic], "Exit codes") {
			t.Errorf("help for %s does not document exit codes", topic)
		}
	}
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := put(t, filepath.Join(dir, "good", "a.png"), samples.PNG().Data)
	unknown := put(t, filepath.Join(dir, "mixed", "notes.txt"), []byte("just some text"))
	put(t, filepath.Join(dir, "mixed", "b.png"), samples.PNG().Data)
	broken := put(t, filepath.Join(dir, "broken", "c.png"), samples.PNG().Data[:40])
	lying := put(t, filepath.Join(dir, "lying", "d.liar"), []byte(liarMagic+"<x:xmpmeta"))

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"everything cleaned", []string{"clean", "--dry-run", good}, exitOK},
		{"missing file", []string{"clean", filepath.Join(dir, "missing.png")}, exitFailed},
		{"damaged file", []string{"clean", broken}, exitFailed},
		{"unsupported, named explicitly", []string{"clean", unknown}, exitFailed},
		{"directory without -r", []string{"clean", filepath.Join(dir, "good")}, exitFailed},
		{"verification failure", []string{"clean", lying}, exitFailed},
		{"unsupported, found in a walk", []string{"clean", "-r", "--dry-run", filepath.Join(dir, "mixed")}, exitIncomplete},
		{"failure beats incomplete", []string{"clean", "-r", "--dry-run", filepath.Join(dir, "mixed"), broken}, exitFailed},
		{"inspect, walk with unsupported", []string{"inspect", "-r", filepath.Join(dir, "mixed")}, exitIncomplete},
		{"inspect, all fine", []string{"inspect", good}, exitOK},
		{"inspect, missing file", []string{"inspect", filepath.Join(dir, "missing.png")}, exitFailed},
		{"empty folder has nothing left undone", []string{"clean", "-r", t.TempDir()}, exitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := hideme(t, c.args...)
			if out.code != c.want {
				t.Errorf("exit %d, want %d\n%s\n%s", out.code, c.want, out.stdout, out.stderr)
			}
			asJSON := hideme(t, append(c.args, "--json")...)
			if doc := parseJSON(t, asJSON.stdout); doc.ExitCode != c.want || asJSON.code != c.want {
				t.Errorf("--json: exit %d, document says %d, want %d", asJSON.code, doc.ExitCode, c.want)
			}
		})
	}
}

// TestExitZeroMeansEveryFileWasCleanedAndVerified looks at it from the script's side:
// run over a folder, trust a zero exit, and check that the promise really held.
func TestExitZeroMeansEveryFileWasCleanedAndVerified(t *testing.T) {
	src := filepath.Join(t.TempDir(), "in")
	all := putAllSamples(t, src)
	out := filepath.Join(t.TempDir(), "out")

	result := hideme(t, "clean", "-r", "-o", out, src)
	if result.code != exitOK {
		t.Fatalf("exit %d\n%s", result.code, result.stdout)
	}
	for name, s := range all {
		data := get(t, filepath.Join(out, "in", name))
		for _, secret := range s.Secrets {
			if bytes.Contains(data, secret) {
				t.Errorf("exit 0, but %s holds %q", name, secret)
			}
		}
	}

	put(t, filepath.Join(src, "extra.txt"), []byte("text"))
	again := hideme(t, "clean", "-r", "-o", filepath.Join(t.TempDir(), "out"), src)
	if again.code != exitIncomplete {
		t.Errorf("a file left uncleaned must not exit 0, got %d", again.code)
	}
	if !strings.Contains(again.stdout, "extra.txt") {
		t.Errorf("the file left uncleaned was not listed:\n%s", again.stdout)
	}
}

func TestLyingHandlerIsCaught(t *testing.T) {
	dir := t.TempDir()
	content := []byte(liarMagic + "<x:xmpmeta>Test Author</x:xmpmeta>\n")
	src := put(t, filepath.Join(dir, "doc.liar"), content)

	for _, args := range [][]string{
		{"clean", src},
		{"clean", "--in-place", src},
		{"clean", "-o", filepath.Join(dir, "out"), src},
	} {
		out := hideme(t, args...)
		if out.code != exitFailed {
			t.Errorf("%q: exit %d", args, out.code)
		}
		if !strings.Contains(out.stdout, "FAILED (verification)") || !strings.Contains(out.stdout, "signature") {
			t.Errorf("%q: stdout:\n%s", args, out.stdout)
		}
		if !bytes.Equal(get(t, src), content) {
			t.Fatalf("%q: the original changed", args)
		}
		if exists(filepath.Join(dir, "doc.clean.liar")) || exists(filepath.Join(dir, "out", "doc.liar")) {
			t.Errorf("%q: the lying handler's output survived", args)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".hideme-") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

func TestInPlace(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "sub", "a.png"), samples.PNG().Data)

	out := hideme(t, "clean", "-r", "--in-place", dir)

	if out.code != exitOK {
		t.Fatalf("exit %d\n%s", out.code, out.stdout)
	}
	if bytes.Contains(get(t, src), []byte(samples.Author)) {
		t.Error("the file still holds the metadata")
	}
	entries, err := os.ReadDir(filepath.Dir(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %v", entries)
	}
}

func TestOutputMirroring(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "photos")
	put(t, filepath.Join(tree, "2024", "a.png"), samples.PNG().Data)
	put(t, filepath.Join(tree, "b.gif"), samples.GIF().Data)
	out := filepath.Join(root, "out")

	result := hideme(t, "clean", "-r", tree, "-o", out)

	if result.code != exitOK {
		t.Fatalf("exit %d\n%s", result.code, result.stdout)
	}
	for _, want := range []string{filepath.Join(out, "photos", "2024", "a.png"), filepath.Join(out, "photos", "b.gif")} {
		if !exists(want) {
			t.Errorf("missing %s\n%s", want, result.stdout)
		}
	}
}

func TestNameCollisionsAndForce(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)

	if out := hideme(t, "clean", src); out.code != exitOK {
		t.Fatalf("first run: exit %d\n%s", out.code, out.stdout)
	}
	output := filepath.Join(dir, "a.clean.png")
	first := get(t, output)

	refused := hideme(t, "clean", src)
	if refused.code != exitFailed || !strings.Contains(refused.stdout, "output-exists") || !strings.Contains(refused.stdout, "--force") {
		t.Errorf("exit %d:\n%s", refused.code, refused.stdout)
	}
	put(t, output, []byte("the user edited this"))
	hideme(t, "clean", src)
	if string(get(t, output)) != "the user edited this" {
		t.Error("an existing output was overwritten without --force")
	}

	forced := hideme(t, "clean", src, "--force")
	if forced.code != exitOK {
		t.Errorf("exit %d:\n%s", forced.code, forced.stdout)
	}
	if !bytes.Equal(get(t, output), first) {
		t.Error("--force did not produce the same clean output")
	}

	// Two inputs meeting at one output: the second is refused, even with --force.
	a := put(t, filepath.Join(dir, "x", "same.png"), samples.PNG().Data)
	b := put(t, filepath.Join(dir, "y", "same.png"), samples.PNG().Data)
	clash := hideme(t, "clean", "--force", "-o", filepath.Join(dir, "merged"), a, b)
	if clash.code != exitFailed || !strings.Contains(clash.stdout, "output-collision") {
		t.Errorf("exit %d:\n%s", clash.code, clash.stdout)
	}
}

func TestRandomName(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "IMG_20240101.png"), samples.PNG().Data)

	doc := parseJSON(t, hideme(t, "clean", "--json", "--random-name", "-o", filepath.Join(dir, "out"), src).stdout)

	output := doc.Results[0].Output
	if strings.Contains(filepath.Base(output), "IMG") || filepath.Ext(output) != ".png" || !exists(output) {
		t.Errorf("output %q", output)
	}
}

func TestDryRun(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)

	out := hideme(t, "clean", "--dry-run", src)

	if out.code != exitOK || !strings.Contains(out.stdout, "would clean") {
		t.Errorf("exit %d:\n%s", out.code, out.stdout)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a dry run left files: %v", entries)
	}
	doc := parseJSON(t, hideme(t, "clean", "--dry-run", "--json", src).stdout)
	if !doc.DryRun || doc.Results[0].Status != "would-clean" || doc.Summary.WouldClean != 1 {
		t.Errorf("document: %+v", doc)
	}
}

func TestWalkingAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := put(t, filepath.Join(root, "outside", "secret.png"), samples.PNG().Data)
	tree := filepath.Join(root, "tree")
	put(t, filepath.Join(tree, "a.png"), samples.PNG().Data)
	put(t, filepath.Join(tree, "earlier.clean.png"), samples.PNG().Data)
	put(t, filepath.Join(tree, "readme.txt"), []byte("text"))
	linked := true
	if err := os.Symlink(outside, filepath.Join(tree, "link.png")); err != nil {
		linked = false
	}

	out := hideme(t, "clean", "-r", tree)

	if out.code != exitIncomplete {
		t.Errorf("exit %d\n%s", out.code, out.stdout)
	}
	if !strings.Contains(out.stdout, "UNSUPPORTED") || !strings.Contains(findLine(out.stdout, "readme.txt"), "UNSUPPORTED") {
		t.Errorf("the unsupported file was not listed:\n%s", out.stdout)
	}
	if !strings.Contains(findLine(out.stdout, "earlier.clean.png"), "SKIPPED") {
		t.Errorf("the earlier output was not listed as skipped:\n%s", out.stdout)
	}
	if exists(filepath.Join(tree, "earlier.clean.clean.png")) {
		t.Error("a re-run made a .clean.clean file")
	}
	if linked {
		if !strings.Contains(findLine(out.stdout, "link.png"), "SKIPPED") {
			t.Errorf("the symlink was not listed as skipped:\n%s", out.stdout)
		}
		if exists(filepath.Join(tree, "link.clean.png")) {
			t.Error("a symlink was followed")
		}
	} else {
		t.Log("symlinks are not available here, so that part was not exercised")
	}
	if !strings.Contains(out.stdout, "skipped: ") || !strings.Contains(out.stdout, "unsupported: 1") {
		t.Errorf("summary:\n%s", out.stdout)
	}
	if !strings.Contains(out.stderr, "not processed") {
		t.Errorf("no note on stderr that files were left: %q", out.stderr)
	}
}

func TestResultsComeInInputOrder(t *testing.T) {
	dir := t.TempDir()
	var args []string
	for _, name := range []string{"z.png", "m.gif", "a.png", "k.gif", "b.png"} {
		data := samples.PNG().Data
		if filepath.Ext(name) == ".gif" {
			data = samples.GIF().Data
		}
		args = append(args, put(t, filepath.Join(dir, name), data))
	}

	doc := parseJSON(t, hideme(t, append([]string{"inspect", "--json", "--jobs", "4"}, args...)...).stdout)

	for i, r := range doc.Results {
		if r.Path != args[i] {
			t.Errorf("result %d is %s, want %s", i, r.Path, args[i])
		}
	}
}

func TestHostileNamesCannotInjectIntoTheTerminal(t *testing.T) {
	dir := t.TempDir()
	// Control characters cannot be in a Windows file name, but a bidi override can,
	// and it is what makes "gpj.exe" display as "exe.jpg".
	name := "photo\u202Egnp.exe.png"
	if runtime.GOOS != "windows" {
		name = "photo\x1b[31m\x07\u202Egnp.exe.png"
	}
	src := put(t, filepath.Join(dir, name), samples.PNG().Data)

	for _, args := range [][]string{
		{"inspect", src},
		{"inspect", "--json", src},
		{"clean", "--dry-run", src},
		{"clean", "--dry-run", "--json", src},
		{"inspect", "-r", dir},
		{"clean", "-r", "--dry-run", dir},
		{"clean", "-o", filepath.Join(dir, "out"), src},
	} {
		out := hideme(t, args...)
		for _, stream := range []string{out.stdout, out.stderr} {
			if strings.ContainsAny(stream, "\x1b\x07\u202E") {
				t.Errorf("%q printed a hostile character:\n%q", args, stream)
			}
		}
		if !strings.Contains(out.stdout, "gnp.exe.png") {
			t.Errorf("%q does not show the name at all:\n%s", args, out.stdout)
		}
		if !strings.Contains(out.stdout, "shown changed") {
			t.Errorf("%q does not warn that the name is shown changed:\n%s", args, out.stdout)
		}
	}
}

func TestHostileArgumentsCannotInjectThroughUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"clean", "-\x1b[31mevil.jpg"},
		{"clean", "--json", "-\x1b[31mevil.jpg"},
		{"\x1b[31mevil"},
		{"clean", "--suffix", "\x1b[31m", "x.png"},
	} {
		out := hideme(t, args...)
		if out.code != exitUsage {
			t.Errorf("%q: exit %d", args, out.code)
		}
		if strings.ContainsRune(out.stdout, 0x1b) || strings.ContainsRune(out.stderr, 0x1b) {
			t.Errorf("%q printed an escape:\nstdout %q\nstderr %q", args, out.stdout, out.stderr)
		}
	}
}

func TestSanitizeHandlesEveryField(t *testing.T) {
	// A result built by hand, because no file on Windows can carry these characters in
	// its name, and this is the guarantee that matters on every platform.
	const evil = "x\x1b]0;owned\x07\u202Ey"
	r := resultWithEveryField(evil)
	var stdout, stderr bytes.Buffer
	notes := &stderrNotes{w: &stderr}
	text := &textReporter{w: &stdout, notes: notes, command: "clean", show: display{showValues: true}}
	text.result(r)
	text.finish(summaryOf(r), exitOK)
	asJSON := &jsonReporter{w: &stdout, notes: notes, command: "clean", show: display{showValues: true}}
	asJSON.begin()
	asJSON.result(r)
	asJSON.finish(summaryOf(r), exitOK)
	for _, stream := range []string{stdout.String(), stderr.String()} {
		if strings.ContainsAny(stream, "\x1b\x07\u202E") {
			t.Errorf("a hostile character got through:\n%q", stream)
		}
	}
}

func TestWarningsAreAlwaysShown(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "form.pdf"), samples.PDFWithFormField())

	check := func(t *testing.T, out outcome) {
		t.Helper()
		if !strings.Contains(out.stdout, "form field values remain") {
			t.Errorf("warning missing from stdout:\n%s", out.stdout)
		}
		if !strings.Contains(out.stderr, "form field values remain") {
			t.Errorf("warning not repeated on stderr:\n%s", out.stderr)
		}
	}
	t.Run("clean text", func(t *testing.T) { check(t, hideme(t, "clean", "--dry-run", src)) })
	t.Run("clean json", func(t *testing.T) { check(t, hideme(t, "clean", "--dry-run", "--json", src)) })
	t.Run("inspect text", func(t *testing.T) { check(t, hideme(t, "inspect", src)) })
	t.Run("inspect json", func(t *testing.T) { check(t, hideme(t, "inspect", "--json", src)) })
	t.Run("inspect redacted", func(t *testing.T) { check(t, hideme(t, "inspect", "--redact", src)) })
}

func TestFormatsCommand(t *testing.T) {
	out := hideme(t, "formats")
	if out.code != exitOK {
		t.Fatalf("exit %d", out.code)
	}
	for _, want := range []string{"jpeg: JPEG image", ".heic", "limitation: ", "file-system metadata: "} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, out.stdout)
		}
	}

	asJSON := hideme(t, "formats", "--json")
	var doc struct {
		Schema  string `json:"schema"`
		Formats []struct {
			ID          string   `json:"id"`
			Extensions  []string `json:"extensions"`
			Limitations []string `json:"limitations"`
		} `json:"formats"`
		Filesystem string `json:"filesystem"`
	}
	if err := json.Unmarshal([]byte(asJSON.stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, asJSON.stdout)
	}
	ids := make([]string, len(doc.Formats))
	for i, f := range doc.Formats {
		ids[i] = f.ID
		if len(f.Extensions) == 0 || f.Limitations == nil {
			t.Errorf("format: %+v", f)
		}
	}
	if !slices.IsSorted(ids) {
		t.Errorf("not in a stable order: %q", ids)
	}
	// Eighteen real handlers and the one this test binary adds.
	if len(ids) != 19 || doc.Filesystem == "" {
		t.Errorf("formats: %q, filesystem %q", ids, doc.Filesystem)
	}
}

func TestVersion(t *testing.T) {
	out := hideme(t, "version")
	if out.code != exitOK || !strings.HasPrefix(out.stdout, "hideme ") {
		t.Errorf("exit %d: %q", out.code, out.stdout)
	}
	version = "1.2.3"
	defer func() { version = "" }()
	if got := hideme(t, "version").stdout; !strings.HasPrefix(got, "hideme 1.2.3 ") {
		t.Errorf("-ldflags value ignored: %q", got)
	}
}

func TestColorIsOnlyForTerminals(t *testing.T) {
	if colorEnabled(&bytes.Buffer{}) {
		t.Error("colour for a buffer")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if colorEnabled(f) {
		t.Error("colour for a file")
	}
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stdout) {
		t.Error("colour despite NO_COLOR")
	}
	out := hideme(t, "inspect", put(t, filepath.Join(t.TempDir(), "a.png"), samples.PNG().Data))
	if strings.Contains(out.stdout, "\x1b") {
		t.Errorf("escape codes in plain output: %q", out.stdout)
	}
}

func TestCancelledRunExitsAsFailed(t *testing.T) {
	dir := t.TempDir()
	src := put(t, filepath.Join(dir, "a.png"), samples.PNG().Data)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"clean", src}, &stdout, &stderr)

	if code != exitFailed || !strings.Contains(stdout.String(), "canceled") {
		t.Errorf("exit %d:\n%s", code, stdout.String())
	}
	if exists(filepath.Join(dir, "a.clean.png")) {
		t.Error("a canceled run left an output")
	}
}

func TestParseFlagsKeepsPathsAndOrder(t *testing.T) {
	fs := newFlagSet("test")
	var verbose bool
	var name string
	fs.BoolVar(&verbose, "v", false, "")
	fs.StringVar(&name, "name", "", "")

	paths, err := parseFlags(fs, []string{"a", "-v", "b", "--name", "n", "c", "--", "-d", "--name"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"a", "b", "c", "-d", "--name"}) {
		t.Errorf("paths: %q", paths)
	}
	if !verbose || name != "n" {
		t.Errorf("verbose %v, name %q", verbose, name)
	}
}

func resultWithEveryField(evil string) engine.Result {
	return engine.Result{
		Path:   evil,
		Output: evil,
		Status: engine.StatusCleaned,
		Format: evil,
		Reason: evil,
		Detail: evil,
		Kind:   "io",
		Findings: []core.Finding{
			{Category: core.CatIdentity, Risk: core.RiskHigh, Field: evil, Value: evil, Where: evil, Action: core.ActionRemove},
			{Category: core.CatTechnical, Risk: core.RiskNone, Field: evil, Value: evil, Where: evil, Action: core.ActionRetain, Reason: evil},
		},
		Warnings: []string{evil},
	}
}

func summaryOf(engine.Result) engine.Summary {
	return engine.Summary{Files: 1, Cleaned: 1, Warnings: 1}
}
