package engine

import (
	"path/filepath"
	"testing"
)

func TestSyncDirDoesNotTryOnWindows(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("syncDir returned %v; directories are not synced on Windows", err)
	}
}
