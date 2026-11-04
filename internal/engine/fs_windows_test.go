package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const zoneText = "[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=https://example.invalid/from\r\nHostUrl=https://example.invalid/photo.good\r\n"

// writeStream writes a real NTFS alternate data stream, skipping the test on a file
// system that has none.
func writeStream(t *testing.T, path, stream, contents string) {
	t.Helper()
	if err := os.WriteFile(path+":"+stream, []byte(contents), 0o644); err != nil {
		t.Skipf("alternate data streams not available here: %v", err)
	}
}

func streamNames(t *testing.T, path string) []string {
	t.Helper()
	streams, err := listStreams(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(streams))
	for i, s := range streams {
		names[i] = s.name
	}
	return names
}

func fieldValue(r Result, field string) (string, bool) {
	for _, f := range r.Findings {
		if f.Field == field {
			return f.Value, true
		}
	}
	return "", false
}

func TestZoneIdentifierIsReported(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodCleaned)
	writeStream(t, src, "Zone.Identifier", zoneText)

	r := Inspect(context.Background(), src, Config{})

	if r.Status != StatusInspected {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if got, _ := fieldValue(r, "Download URL"); got != "https://example.invalid/photo.good" {
		t.Errorf("URL %q; findings %+v", got, r.Findings)
	}
	if got, _ := fieldValue(r, "Download referrer URL"); got != "https://example.invalid/from" {
		t.Errorf("referrer %q", got)
	}
	for _, f := range r.Findings {
		if f.Field == "Download URL" && (f.Risk != "high" || f.Category != "filesystem") {
			t.Errorf("finding: %+v", f)
		}
	}
	if r.NothingFound() {
		t.Error("a file with a download URL was reported as holding nothing")
	}
}

func TestOtherAlternateStreamsAreReported(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodCleaned)
	writeStream(t, src, "com.dropbox.attrs", "Test Account")

	r := Inspect(context.Background(), src, Config{})

	got, ok := fieldValue(r, "Alternate data stream")
	if !ok || !strings.Contains(got, "com.dropbox.attrs") || !strings.Contains(got, "12 bytes") {
		t.Errorf("value %q; findings %+v", got, r.Findings)
	}
}

func TestOutputInheritsNoStreams(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	writeStream(t, src, "Zone.Identifier", zoneText)
	writeStream(t, src, "extra", "Test Hidden")

	r := Clean(context.Background(), src, defaultConfig())

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if _, ok := fieldValue(r, "Download URL"); !ok {
		t.Errorf("Clean did not report the stream: %+v", r.Findings)
	}
	if names := streamNames(t, r.Output); len(names) != 0 {
		t.Errorf("output has streams: %q", names)
	}
	if names := streamNames(t, src); !slices.Contains(names, "Zone.Identifier") {
		t.Errorf("the original lost its stream without --in-place: %q", names)
	}
}

func TestInPlaceRemovesStreams(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	writeStream(t, src, "Zone.Identifier", zoneText)

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if _, ok := fieldValue(r, "Download URL"); !ok {
		t.Error("the stream went unreported, and now the evidence is gone")
	}
	if names := streamNames(t, src); len(names) != 0 {
		t.Errorf("streams survived --in-place: %q", names)
	}
	if _, err := os.Stat(src + ":Zone.Identifier"); err == nil {
		t.Error("the Zone.Identifier stream can still be opened")
	}
}

func TestForceReplacesStreams(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	out := write(t, filepath.Join(dir, "photo.clean.good"), "old")
	writeStream(t, out, "Zone.Identifier", zoneText)

	cfg := defaultConfig()
	cfg.Force = true
	r := Clean(context.Background(), src, cfg)

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if names := streamNames(t, out); len(names) != 0 {
		t.Errorf("the replaced output's streams survived: %q", names)
	}
}

func TestStreamPathCannotBeAnOutputName(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "photo.good"), goodFile)
	writeStream(t, src, "payload", goodFile)

	r := Clean(context.Background(), src+":payload", defaultConfig())

	if r.Status != StatusFailed || r.Kind != KindInvalidInput {
		t.Fatalf("status %q kind %q output %q", r.Status, r.Kind, r.Output)
	}
}

func creationTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		t.Fatalf("stat holds %T", info.Sys())
	}
	return time.Unix(0, data.CreationTime.Nanoseconds())
}

var longAgo = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

func TestZeroSetsCreationTime(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		dir := t.TempDir()
		src := write(t, filepath.Join(dir, "a.good"), goodFile)
		if err := setCreationTime(src, longAgo); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Suffix: DefaultSuffix, Timestamp: TimestampZero}
		if inPlace {
			cfg = Config{InPlace: true, Timestamp: TimestampZero}
		}

		r := Clean(context.Background(), src, cfg)

		if r.Status != StatusCleaned {
			t.Fatalf("status %q: %s", r.Status, r.Reason)
		}
		if got := creationTime(t, r.Output); !got.Equal(ZeroTime) {
			t.Errorf("in place %v: created %v", inPlace, got)
		}
	}
}

func TestInPlaceDoesNotKeepTheOriginalsCreationTime(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	if err := setCreationTime(src, longAgo); err != nil {
		t.Fatal(err)
	}

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if got := creationTime(t, src); time.Since(got) > time.Hour {
		t.Errorf("created %v: the original's time was inherited", got)
	}
}

// TestRecreatedOutputDoesNotInheritCreationTime is the case NTFS tunneling really does
// reach: an output deleted moments ago and made again under the same name takes on the
// deleted file's creation time, unless it is forced after the rename.
func TestRecreatedOutputDoesNotInheritCreationTime(t *testing.T) {
	for _, c := range []struct {
		name      string
		timestamp Timestamp
		check     func(time.Time) bool
	}{
		{"now", TimestampNow, func(got time.Time) bool { return time.Since(got) < time.Hour }},
		{"zero", TimestampZero, func(got time.Time) bool { return got.Equal(ZeroTime) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := write(t, filepath.Join(dir, "a.good"), goodFile)
			out := write(t, filepath.Join(dir, "a.clean.good"), "an earlier output")
			if err := setCreationTime(out, longAgo); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(out); err != nil {
				t.Fatal(err)
			}
			cfg := defaultConfig()
			cfg.Timestamp = c.timestamp

			r := Clean(context.Background(), src, cfg)

			if r.Status != StatusCleaned {
				t.Fatalf("status %q: %s", r.Status, r.Reason)
			}
			if got := creationTime(t, out); !c.check(got) {
				t.Errorf("created %v", got)
			}
		})
	}
}

func TestKeepCopiesCreationTime(t *testing.T) {
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.good"), goodFile)
	if err := setCreationTime(src, longAgo); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Timestamp = TimestampKeep

	r := Clean(context.Background(), src, cfg)

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if got := creationTime(t, r.Output); !got.Equal(longAgo) {
		t.Errorf("created %v", got)
	}
}

// makeJunction creates an NTFS junction, which unlike a symlink needs no privilege, so
// these tests always run on Windows.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction: %v: %s", err, out)
	}
}

func TestWalkDoesNotEnterJunctions(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "secret.good"), goodFile)
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "a.good"), goodFile)
	makeJunction(t, filepath.Join(tree, "junction"), outside)

	results, summary := runAll(t, OpClean, []string{tree}, Config{Suffix: DefaultSuffix, Recursive: true})

	if summary.Cleaned != 1 || summary.Skipped != 1 {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
	if exists(filepath.Join(outside, "secret.clean.good")) {
		t.Error("a junction was followed")
	}
}

func TestOutputDirJunctionCannotRedirectWrites(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, filepath.Join(out, "tree"), outside)
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "deep", "a.good"), goodFile)

	results, _ := runAll(t, OpClean, []string{tree}, Config{OutputDir: out, Recursive: true})

	if len(results) != 1 || results[0].Status != StatusFailed || results[0].Kind != KindInvalidInput {
		t.Fatalf("results: %+v", results)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the write went through the junction and left %v", entries)
	}
}

func TestWildcardsExpandOnWindows(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.good"), goodFile)
	write(t, filepath.Join(dir, "b.good"), goodFile)

	results, summary := runAll(t, OpClean, []string{filepath.Join(dir, "*.good")}, defaultConfig())
	if summary.Cleaned != 2 {
		t.Fatalf("results: %+v", results)
	}

	results, summary = runAll(t, OpClean, []string{filepath.Join(dir, "*.nothing")}, defaultConfig())
	if summary.Failed != 1 {
		t.Fatalf("a pattern matching nothing must fail: %+v", results)
	}
}
