package observe

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The cmake --install case: a new directory tree created MID-BUILD
// with files written into it whose mtimes are preserved (old). The
// write-tracker must follow the new directories and record the files
// (found live: dep receipts restored libs but no headers).
func TestWritesFollowsNewDirectories(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("in"), 0o644)

	w := NewWrites(dir)
	if !w.Active() {
		t.Skip("fsnotify unavailable")
	}
	defer w.Close()

	// simulate an installer: mkdir -p pkg/include, write a header
	// with an OLD mtime
	os.MkdirAll(filepath.Join(dir, "pkg/include/mbedtls"), 0o755)
	hdr := filepath.Join(dir, "pkg/include/mbedtls/version.h")
	os.WriteFile(hdr, []byte("#define V 1\n"), 0o644)
	old := time.Now().Add(-365 * 24 * time.Hour)
	os.Chtimes(hdr, old, old)

	// let the watcher drain events
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.Files(dir)) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	files := w.Files(dir)
	found := false
	for _, f := range files {
		if f == "pkg/include/mbedtls/version.h" {
			found = true
		}
	}
	if !found {
		t.Fatalf("file written into a mid-build-created directory must be tracked; got %v", files)
	}
}
