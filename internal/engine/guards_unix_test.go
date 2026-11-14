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
// The size and time check reads the open file, which the rename leaves untouched, so only
// the identity check against the path can catch the swap.
func TestSourceSwappedDuringCleanIsRefused(t *testing.T) {
	dir := t.TempDir()
	const contents = "FAKEHOOKED\ncontent\n"
	const other = "FAKEHOOKED\nOTHER!!\n"
	src := write(t, filepath.Join(dir, "swapped.bin"), contents)
	cleanHook = func(context.Context) error {
		return os.Rename(write(t, filepath.Join(dir, "replacement"), other), src)
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
