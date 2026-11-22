package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPutManifestCAContentAddressed(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	m := &Manifest{
		Key:     "testkey",
		Command: "make",
		Blobs:   []BlobRef{{Hash: "abc", Path: "out.bin", Size: 100}},
	}
	if err := s.PutManifestCA(m); err != nil {
		t.Fatal(err)
	}

	// The manifest should be at manifests/testkey/<hash>.json
	entries, _ := os.ReadDir(filepath.Join(dir, "manifests", "testkey"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	if !jsonExt(entries[0].Name()) {
		t.Fatalf("expected .json file, got %s", entries[0].Name())
	}

	// Verify it can be read back
	got, err := s.GetManifestCA("testkey")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "make" {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestPutManifestCADifferentContentDifferentPath(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	// Two manifests with the same key but different content
	m1 := &Manifest{
		Key:     "K",
		Command: "make",
		Blobs:   []BlobRef{{Hash: "abc", Path: "out.bin", Size: 100}},
	}
	m2 := &Manifest{
		Key:     "K",
		Command: "make",
		Blobs:   []BlobRef{{Hash: "def", Path: "out.bin", Size: 200}}, // different blob
	}

	if err := s.PutManifestCA(m1); err != nil {
		t.Fatal(err)
	}
	if err := s.PutManifestCA(m2); err != nil {
		t.Fatal(err)
	}

	// Both should exist (different content → different hash → different path)
	entries, _ := os.ReadDir(filepath.Join(dir, "manifests", "K"))
	if len(entries) != 2 {
		t.Fatalf("expected 2 files (different content), got %d", len(entries))
	}
}

func TestPutManifestCASameContentIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	m := &Manifest{
		Key:     "K",
		Command: "make",
		Blobs:   []BlobRef{{Hash: "abc", Path: "out.bin", Size: 100}},
	}

	// Write the same manifest twice — should succeed (idempotent)
	if err := s.PutManifestCA(m); err != nil {
		t.Fatal(err)
	}
	if err := s.PutManifestCA(m); err != nil {
		t.Fatal(err)
	}

	// Only one file (same content → same hash → same path → O_EXCL skips)
	entries, _ := os.ReadDir(filepath.Join(dir, "manifests", "K"))
	if len(entries) != 1 {
		t.Fatalf("idempotent write: expected 1 file, got %d", len(entries))
	}
}

func TestGetManifestCADivergence(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	// Two manifests with the same key but different outputs
	s.PutManifestCA(&Manifest{
		Key: "K", Command: "make",
		Blobs: []BlobRef{{Hash: "abc", Path: "out.bin"}},
	})
	s.PutManifestCA(&Manifest{
		Key: "K", Command: "make",
		Blobs: []BlobRef{{Hash: "def", Path: "out.bin"}},
	})

	// Reading should fail with a divergence error
	_, err := s.GetManifestCA("K")
	if err == nil {
		t.Fatal("expected divergence error")
	}
	if !containsStr2(err.Error(), "DIVERGENCE") {
		t.Fatalf("expected DIVERGENCE in error, got: %v", err)
	}
	if !containsStr2(err.Error(), "2") {
		t.Fatalf("expected count 2, got: %v", err)
	}
}

func TestGetManifestCAMiss(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	_, err := s.GetManifestCA("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestHasManifestCA(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	if s.HasManifestCA("K") {
		t.Fatal("empty store should not have manifests")
	}

	s.PutManifestCA(&Manifest{Key: "K", Command: "make"})
	if !s.HasManifestCA("K") {
		t.Fatal("should have manifest after put")
	}
	if s.HasManifestCA("other") {
		t.Fatal("should not have other key")
	}
}

func TestListManifestHashes(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	s.PutManifestCA(&Manifest{Key: "K", Command: "make",
		Blobs: []BlobRef{{Hash: "abc"}}})
	s.PutManifestCA(&Manifest{Key: "K", Command: "make",
		Blobs: []BlobRef{{Hash: "def"}}})

	hashes := s.ListManifestHashes("K")
	if len(hashes) != 2 {
		t.Fatalf("expected 2 hashes, got %d", len(hashes))
	}
	if hashes[0] == hashes[1] {
		t.Fatal("hashes should be different (different content)")
	}
}

func TestManifestSelfHashDeterministic(t *testing.T) {
	m1 := &Manifest{Key: "K", Command: "make", Blobs: []BlobRef{{Hash: "abc"}}}
	m2 := &Manifest{Key: "K", Command: "make", Blobs: []BlobRef{{Hash: "abc"}}}
	if manifestSelfHash(m1) != manifestSelfHash(m2) {
		t.Fatal("same content should hash the same")
	}
	m3 := &Manifest{Key: "K", Command: "make", Blobs: []BlobRef{{Hash: "xyz"}}}
	if manifestSelfHash(m1) == manifestSelfHash(m3) {
		t.Fatal("different content should hash differently")
	}
}

func jsonExt(name string) bool {
	return len(name) > 5 && name[len(name)-5:] == ".json"
}

func containsStr2(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
