package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A name ending in a dot or space exists on NTFS but cannot be reached by ordinary
// paths, where Win32 strips the ending and so finds a different file or none. A walk
// that went on through the bare name dropped such files without a word: exit 0.
func TestWalkFailsOnNamesAPathCannotReach(t *testing.T) {
	dir := t.TempDir()
	root := `\\?\` + dir
	t.Cleanup(func() { os.RemoveAll(root) })
	put := func(name ...string) string {
		path := root + `\` + strings.Join(name, `\`)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(goodFile), 0o644); err != nil {
			t.Skipf("cannot create such names here: %v", err)
		}
		return path
	}
	ok := put("a.good")
	hidden := []string{put("a.good."), put("b.good "), put("sub.", "inner.good")}
	// sub. reads as sub, which is empty, so nothing would show that anything was missed.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	results, summary := runAll(t, OpClean, []string{dir}, Config{InPlace: true, Recursive: true})

	if summary.Cleaned != 1 || summary.Failed != 3 {
		t.Errorf("summary %+v: %+v", summary, results)
	}
	for _, r := range results {
		if r.Status == StatusFailed && r.Kind != KindInvalidInput {
			t.Errorf("%+v", r)
		}
	}
	if strings.Contains(read(t, ok), "SECRET=") {
		t.Error("the reachable file was not cleaned")
	}
	for _, path := range hidden {
		if !strings.Contains(read(t, path), "SECRET=") {
			t.Errorf("%s changed", path)
		}
	}
}

// Replacing a file through another spelling of its name must not rename it. A short
// name took the real long name with it for good.
func TestInPlaceKeepsTheNameOnDisk(t *testing.T) {
	t.Run("other case", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "photo.good"), goodFile)

		r := Clean(context.Background(), filepath.Join(dir, "PHOTO.GOOD"), Config{InPlace: true})

		if r.Status != StatusCleaned {
			t.Fatalf("%q: %s", r.Status, r.Reason)
		}
		if got := listNames(t, dir); !slices.Equal(got, []string{"photo.good"}) {
			t.Errorf("directory holds %q", got)
		}
	})
	t.Run("short name", func(t *testing.T) {
		dir := t.TempDir()
		const long = "a rather long file name with spaces in it.good"
		src := write(t, filepath.Join(dir, long), goodFile)
		short := make([]uint16, 1024)
		p, err := syscall.UTF16PtrFromString(src)
		if err != nil {
			t.Fatal(err)
		}
		n, err := syscall.GetShortPathName(p, &short[0], uint32(len(short)))
		name := filepath.Base(syscall.UTF16ToString(short[:n]))
		if err != nil || strings.EqualFold(name, long) {
			t.Skip("this volume has no short names")
		}

		r := Clean(context.Background(), filepath.Join(dir, name), Config{InPlace: true})

		if r.Status != StatusCleaned {
			t.Fatalf("%q: %s", r.Status, r.Reason)
		}
		if got := listNames(t, dir); !slices.Equal(got, []string{long}) {
			t.Errorf("directory holds %q, the long name is gone", got)
		}
		if strings.Contains(read(t, src), "SECRET=") {
			t.Error("not cleaned")
		}
	})
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// PowerShell and cmd.exe hand "*.jpg" over unexpanded, and on Windows that pattern has
// always matched IMG_0001.JPG. Matching it case-sensitively left the camera's files
// uncleaned and unmentioned, with exit 0.
func TestWildcardsMatchTheWayWindowsDoes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"IMG_0001.GOOD", "photo.good", "Mixed.GoOd", "other.txt"} {
		write(t, filepath.Join(dir, name), goodFile)
	}
	bracketed := filepath.Join(dir, "x[1]")
	write(t, filepath.Join(bracketed, "a[2].good"), goodFile)

	results, _ := runAll(t, OpInspect, []string{filepath.Join(dir, "*.good")}, Config{})
	if len(results) != 3 {
		t.Errorf("*.good matched %+v", results)
	}
	results, _ = runAll(t, OpInspect, []string{filepath.Join(dir, "*.GOOD")}, Config{})
	if len(results) != 3 {
		t.Errorf("*.GOOD matched %+v", results)
	}
	results, _ = runAll(t, OpInspect, []string{filepath.Join(bracketed, "a[2]*")}, Config{})
	if len(results) != 1 || results[0].Status != StatusInspected {
		t.Errorf("brackets are ordinary characters on Windows: %+v", results)
	}
	results, _ = runAll(t, OpInspect, []string{`\\?\` + filepath.Join(dir, "photo.good")}, Config{})
	if len(results) != 1 || results[0].Status != StatusInspected {
		t.Errorf("the ? of a prefix is no wildcard: %+v", results)
	}
	results, _ = runAll(t, OpInspect, []string{filepath.Join(dir, "*.nothing")}, Config{})
	if len(results) != 1 || results[0].Kind != KindNotFound || !strings.Contains(results[0].Reason, "no file matches") {
		t.Errorf("a pattern matching nothing: %+v", results)
	}
}

// protectFile gives a file one entry, for the current user, with nothing inherited.
func protectFile(t *testing.T, path string) {
	t.Helper()
	user := os.Getenv("USERNAME")
	if out, err := exec.Command("icacls", path, "/inheritance:r", "/grant:r", user+":F").CombinedOutput(); err != nil || user == "" {
		t.Skipf("cannot set permissions here: %v: %s", err, out)
	}
}

// aclOf says what a file's permissions are, in the terms the tests care about.
func aclOf(t *testing.T, path string) (protected bool, entries int) {
	t.Helper()
	sd, err := readDACL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.free()
	return sd.protected(), sd.aceCount()
}

// The temp file inherits from the folder, and a private file in a shared folder came
// out of --in-place readable by everyone the folder lets read.
func TestPrivatePermissionsSurviveCleaning(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", shared, "/grant", "*S-1-5-32-545:(OI)(CI)(R)").CombinedOutput(); err != nil {
		t.Skipf("cannot set permissions here: %v: %s", err, out)
	}

	t.Run("in place", func(t *testing.T) {
		src := write(t, filepath.Join(shared, "private.good"), goodFile)
		protectFile(t, src)
		r := Clean(context.Background(), src, Config{InPlace: true})
		if r.Status != StatusCleaned {
			t.Fatalf("%q: %s", r.Status, r.Reason)
		}
		if protected, entries := aclOf(t, src); !protected || entries != 1 {
			t.Errorf("the private file is no longer private: protected %v, %d entries", protected, entries)
		}
	})
	t.Run("a copy", func(t *testing.T) {
		src := write(t, filepath.Join(shared, "private2.good"), goodFile)
		protectFile(t, src)
		r := Clean(context.Background(), src, defaultConfig())
		if r.Status != StatusCleaned {
			t.Fatalf("%q: %s", r.Status, r.Reason)
		}
		if protected, entries := aclOf(t, r.Output); !protected || entries != 1 {
			t.Errorf("the copy of a private file is not private: protected %v, %d entries", protected, entries)
		}
	})
	t.Run("an extra entry of its own", func(t *testing.T) {
		src := write(t, filepath.Join(shared, "extra.good"), goodFile)
		if out, err := exec.Command("icacls", src, "/grant", "*S-1-5-32-546:(R)").CombinedOutput(); err != nil {
			t.Skipf("cannot set permissions here: %v: %s", err, out)
		}
		_, before := aclOf(t, src)
		r := Clean(context.Background(), src, Config{InPlace: true})
		if r.Status != StatusCleaned {
			t.Fatalf("%q: %s", r.Status, r.Reason)
		}
		if protected, entries := aclOf(t, src); protected || entries != before {
			t.Errorf("protected %v, %d entries, want %d", protected, entries, before)
		}
	})
	t.Run("an ordinary file still follows its folder", func(t *testing.T) {
		src := write(t, filepath.Join(shared, "plain.good"), goodFile)
		_, before := aclOf(t, src)
		r := Clean(context.Background(), src, Config{InPlace: true})
		if r.Status != StatusCleaned || len(r.Warnings) != 0 {
			t.Fatalf("%q: %s %q", r.Status, r.Reason, r.Warnings)
		}
		if protected, entries := aclOf(t, src); protected || entries != before {
			t.Errorf("protected %v, %d entries, want %d", protected, entries, before)
		}
	})
}

func TestInPlaceKeepsAHiddenFileHidden(t *testing.T) {
	src := write(t, filepath.Join(t.TempDir(), "a.good"), goodFile)
	p, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusCleaned {
		t.Fatalf("%q: %s", r.Status, r.Reason)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&syscall.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Error("--in-place unhid the file")
	}
}

func TestStreamsPastTheLimitAreAdmitted(t *testing.T) {
	src := write(t, filepath.Join(t.TempDir(), "a.good"), goodCleaned)
	for i := range maxStreams + 2 {
		writeStream(t, src, "s"+strconv.Itoa(i), "x")
	}

	r := Inspect(context.Background(), src, Config{})

	if r.Status != StatusInspected || len(r.Findings) != maxStreams {
		t.Fatalf("%q, %d findings", r.Status, len(r.Findings))
	}
	if !slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, "streams") }) {
		t.Errorf("a partial list with no warning: %q", r.Warnings)
	}
}

// A stream's name is the user's to choose, so it is a value and not a location: clean
// --json and inspect --redact both print "where".
func TestStreamNameStaysOutOfTheLocation(t *testing.T) {
	src := write(t, filepath.Join(t.TempDir(), "a.good"), goodCleaned)
	writeStream(t, src, "Test Author notes", "x")

	r := Inspect(context.Background(), src, Config{})

	if got, _ := fieldValue(r, "Alternate data stream"); !strings.Contains(got, "Test Author notes") {
		t.Errorf("inspect should show the name as the value: %q", got)
	}
	for _, f := range r.Findings {
		if strings.Contains(f.Where, "Test Author") || strings.Contains(f.Field, "Test Author") {
			t.Errorf("the name is outside the value: %+v", f)
		}
	}
}

func TestNamedPathWithAnUnreachableElementIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x", "a.good"), goodFile)
	write(t, filepath.Join(dir, "a.good"), goodFile)
	for _, path := range []string{
		filepath.Join(dir, "a.good") + ".",
		filepath.Join(dir, "a.good") + " ",
		filepath.Join(dir, "x") + `.\a.good`,
	} {
		if r := Clean(context.Background(), path, Config{InPlace: true}); r.Status != StatusFailed || r.Kind != KindInvalidInput {
			t.Errorf("%q: %q %q %s", path, r.Status, r.Kind, r.Reason)
		}
	}
	if !strings.Contains(read(t, filepath.Join(dir, "x", "a.good")), "SECRET=") {
		t.Error("a file was cleaned by a name that is not its own")
	}
	// "." and ".." are ordinary.
	here := dir + `\x\..\.\a.good`
	if r := Inspect(context.Background(), here, Config{}); r.Status != StatusInspected {
		t.Errorf("%q: %q %s", here, r.Status, r.Reason)
	}
}

func TestDryRunRefusesAJunctionInTheOutputDir(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	out := filepath.Join(root, "out")
	for _, dir := range []string{outside, out} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makeJunction(t, filepath.Join(out, "tree"), outside)
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "a.good"), goodFile)

	results, _ := runAll(t, OpClean, []string{tree}, Config{OutputDir: out, Recursive: true, DryRun: true})

	if results[0].Status != StatusFailed || results[0].Kind != KindInvalidInput {
		t.Errorf("a dry run must refuse what a real run refuses: %+v", results[0])
	}
}
