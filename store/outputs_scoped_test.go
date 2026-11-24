package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotOutputsScoped(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, ".brine"))
	if err != nil {
		t.Fatal(err)
	}
	readSet := []string{"main.c"}
	if err := os.WriteFile(filepath.Join(dir, "main.c"), []byte("int main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	builtAt := time.Now().UnixNano()

	// Build "produces" a binary, an object file, and a depfile.
	artifacts := map[string]string{
		"build/app":   "BINARY",
		"build/app.o": "OBJECT",
		"build/app.d": "DEPFILE",
	}
	for rel, content := range artifacts {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		// mtime > builtAt: written during the build
		future := time.Now().Add(time.Second)
		os.Chtimes(p, future, future)
	}

	// Unscoped: every artifact.
	m, err := s.SnapshotOutputs("k-all", dir, readSet, builtAt, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 3 {
		t.Fatalf("unscoped snapshot: %d blobs, want 3", len(m.Blobs))
	}

	// Scoped to the deliverable: only build/app.
	m, err = s.SnapshotOutputs("k-scoped", dir, readSet, builtAt, []string{"build/app"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 1 || m.Blobs[0].Path != "build/app" {
		t.Fatalf("scoped snapshot: %+v, want only build/app", m.Blobs)
	}
	if len(m.OutputPatterns) != 1 || m.OutputPatterns[0] != "build/app" {
		t.Fatalf("manifest must record its output_patterns: %v", m.OutputPatterns)
	}

	// Glob scope.
	m, err = s.SnapshotOutputs("k-glob", dir, readSet, builtAt, []string{"*.o"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 1 || m.Blobs[0].Path != "build/app.o" {
		t.Fatalf("glob snapshot: %+v, want only build/app.o", m.Blobs)
	}

	// Scoped pattern that matches nothing the build produced.
	m, err = s.SnapshotOutputs("k-none", dir, readSet, builtAt, []string{"nonexistent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 0 {
		t.Fatalf("no-match snapshot: %d blobs, want 0", len(m.Blobs))
	}
}
