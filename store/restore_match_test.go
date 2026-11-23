package store

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strings2reader(s string) io.Reader {
	return strings.NewReader(s)
}

func TestRestoreManifestMatching(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, ".brine"))
	if err != nil {
		t.Fatal(err)
	}

	// Build a manifest with blobs at varied paths.
	m := &Manifest{Key: "testmatch", BuiltAt: 1}
	for _, p := range []string{
		"cmake-build/collector/collector",
		"cmake-build/collector/self-checks",
		"cmake-build/libscap/scap.o",
		"out/binary",
	} {
		hash, _, err := s.PutBlob(strings2reader("content-of-" + p))
		if err != nil {
			t.Fatal(err)
		}
		m.Blobs = append(m.Blobs, BlobRef{Hash: hash, Path: p, Size: int64(len("content-of-" + p))})
	}

	target := t.TempDir()

	// Substring match: "collector" hits both collector-dir outputs.
	n, _, err := s.RestoreManifestMatching(m, target, "collector")
	if err != nil || n != 2 {
		t.Fatalf("substring 'collector': n=%d err=%v, want 2", n, err)
	}
	if _, err := os.Stat(filepath.Join(target, "cmake-build/collector/collector")); err != nil {
		t.Errorf("substring match did not restore cmake-build/collector/collector: %v", err)
	}

	// Glob match: "*.o" hits the object file (base-name match).
	n, _, err = s.RestoreManifestMatching(m, target, "*.o")
	if err != nil || n != 1 {
		t.Fatalf("glob '*.o': n=%d err=%v, want 1", n, err)
	}

	// Glob on full path.
	n, _, err = s.RestoreManifestMatching(m, target, "out/*")
	if err != nil || n != 1 {
		t.Fatalf("glob 'out/*': n=%d err=%v, want 1", n, err)
	}

	// No match = error.
	if _, _, err := s.RestoreManifestMatching(m, target, "nonexistent"); err == nil {
		t.Fatalf("no-match pattern must error")
	}

	// Full path as substring = exact restore.
	n, _, err = s.RestoreManifestMatching(m, target, "cmake-build/collector/collector")
	if err != nil || n != 1 {
		t.Fatalf("exact path: n=%d err=%v, want 1", n, err)
	}
}
