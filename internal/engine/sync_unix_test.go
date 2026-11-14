//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncDirReportsAFolderItCannotOpen(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("syncDir returned nil for a folder that does not exist")
	}
}

func TestFailedFolderSyncIsAWarningNotAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any folder, so mode 0300 cannot make the open fail")
	}
	src := write(t, filepath.Join(t.TempDir(), "a.bin"), "FAKEHOOKED\n")
	// Write and search without read: the output can be created in it and renamed,
	// but opening it to sync it fails.
	out := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(out, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(out, 0o700) })

	r := Clean(context.Background(), src, Config{Suffix: DefaultSuffix, OutputDir: out})

	if r.Status != StatusCleaned {
		t.Fatalf("status %q: %s", r.Status, r.Reason)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "may not survive a power cut") {
		t.Errorf("warnings: %q", r.Warnings)
	}
}
