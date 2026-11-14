//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A swap is only possible where an open file can be renamed over, which Windows forbids.
// The replacement has the same size and modification time, so only the identity check
// can tell it from the file that was read.
func TestSourceSwappedDuringCleanIsRefused(t *testing.T) {
	dir := t.TempDir()
	const contents = "FAKEHOOKED\ncontent\n"
	const other = "FAKEHOOKED\nOTHER!!\n"
	src := write(t, filepath.Join(dir, "swapped.bin"), contents)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	cleanHook = func(context.Context) error {
		repl := write(t, filepath.Join(dir, "replacement"), other)
		if err := os.Chtimes(repl, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		return os.Rename(repl, src)
	}
	defer func() { cleanHook = nil }()

	r := Clean(context.Background(), src, Config{InPlace: true})

	if r.Status != StatusFailed || r.Kind != KindInvalidInput {
		t.Fatalf("status %q kind %q: %s", r.Status, r.Kind, r.Reason)
	}
	if !strings.Contains(r.Reason, "the file was replaced while it was being cleaned") {
		t.Errorf("reason %q does not name the guard", r.Reason)
	}
	if got := read(t, src); got != other {
		t.Errorf("the other program's file was overwritten: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %v (err %v), want only the source", entries, err)
	}
	assertNoTemp(t, dir)
}
