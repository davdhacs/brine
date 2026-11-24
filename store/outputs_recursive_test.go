package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The libtool .libs case: a hidden dir under a "/**" scope must NOT
// be pruned even with an old mtime (restored by the incremental path)
// — else the chain loses the shared library every generation.
func TestSnapshotOutputsHiddenDirUnderScope(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(filepath.Join(dir, ".brine"))
	readSet := []string{"input.txt"}
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("in"), 0o644)
	builtAt := time.Now().UnixNano()

	// .libs file with an OLD mtime (restored state)
	so := filepath.Join(dir, "bld/lib/.libs/libcurl.so")
	os.MkdirAll(filepath.Dir(so), 0o755)
	os.WriteFile(so, []byte("SHARED LIB"), 0o644)
	old := time.Now().Add(-24 * time.Hour)
	os.Chtimes(so, old, old)
	// the .libs dir itself: old mtime too
	os.Chtimes(filepath.Dir(so), old, old)

	// scoped + write-tracked (the REAL incremental flow: the restore
	// writes fire the fsnotify tracker): captured through the hidden dir
	written := map[string]bool{"bld/lib/.libs/libcurl.so": true}
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, []string{"bld/**"}, written)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range m.Blobs {
		if b.Path == "bld/lib/.libs/libcurl.so" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bld/** + write-tracked must capture the hidden .libs contents: %+v", m.Blobs)
	}

	// scoped but NOT write-tracked (pre-existing old file): the mtime
	// rule correctly skips it (the scope covers the dir, not the
	// freshness decision)
	m1b, err := s.SnapshotOutputs("k1b", dir, readSet, builtAt, []string{"bld/**"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m1b.Blobs {
		if b.Path == "bld/lib/.libs/libcurl.so" {
			t.Fatalf("old-mtime untracked file must be skipped by the mtime rule even under scope")
		}
	}

	// unscoped: hidden dir with old mtime still pruned (VCS, caches)
	m2, err := s.SnapshotOutputs("k2", dir, readSet, builtAt, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m2.Blobs {
		if b.Path == "bld/lib/.libs/libcurl.so" {
			t.Fatalf("unscoped snapshot must still prune old hidden dirs")
		}
	}
}
func TestMatchOutputScopeRecursive(t *testing.T) {
	cases := map[string]struct {
		patterns []string
		rel      string
		want     bool
	}{
		"dir-starstar nested":  {[]string{"pkg/**"}, "pkg/mbedtls/include/mbedtls/aes.h", true},
		"dir-starstar direct":  {[]string{"pkg/**"}, "pkg/file.txt", true},
		"dir-starstar outside": {[]string{"pkg/**"}, "other/file.txt", false},
		"dir-starstar exact":   {[]string{"pkg/**"}, "pkg", false},
		"mid-starstar":         {[]string{"bld/**/curl"}, "bld/src/curl", true},
		"mid-starstar no":      {[]string{"bld/**/curl"}, "bld/src/tool_curl", false},
		"single-star one seg":  {[]string{"pkg/*"}, "pkg/mbedtls", true},
		"single-star nested":   {[]string{"pkg/*"}, "pkg/mbedtls/aes.h", false},
		"exact":                {[]string{"bld/src/curl"}, "bld/src/curl", true},
		"exact no":             {[]string{"bld/src/curl"}, "bld/src/curlx", false},
	}
	for name, tc := range cases {
		if got := matchOutputScope(tc.patterns, tc.rel); got != tc.want {
			t.Errorf("%s: matchOutputScope(%v, %q) = %v, want %v", name, tc.patterns, tc.rel, got, tc.want)
		}
	}
}

func TestSnapshotOutputsWrittenSetBeatsMtime(t *testing.T) {
	// cmake --install (and tar, cp -p, etc.) preserve SOURCE mtimes:
	// a header installed during the build carries the tarball's 2024
	// timestamp. mtime-based output capture drops it (found live:
	// dep receipts restored libs but no headers); the write-tracker
	// set must capture it.
	dir := t.TempDir()
	s, _ := New(filepath.Join(dir, ".brine"))
	readSet := []string{"input.txt"}
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("in"), 0o644)
	builtAt := time.Now().UnixNano()

	// installed file with an OLD mtime (pre-build), but in the write set
	hdr := filepath.Join(dir, "pkg/include/version.h")
	os.MkdirAll(filepath.Dir(hdr), 0o755)
	os.WriteFile(hdr, []byte("#define V 1\n"), 0o644)
	old := time.Now().Add(-365 * 24 * time.Hour)
	os.Chtimes(hdr, old, old)

	written := map[string]bool{"pkg/include/version.h": true}
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, nil, written)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range m.Blobs {
		if b.Path == "pkg/include/version.h" {
			found = true
		}
	}
	if !found {
		t.Fatalf("write-tracked file with preserved mtime must be captured as an output: %+v", m.Blobs)
	}

	// without the write set, the old-mtime file is (correctly) skipped
	m2, err := s.SnapshotOutputs("k2", dir, readSet, builtAt, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m2.Blobs {
		if b.Path == "pkg/include/version.h" {
			t.Fatalf("old-mtime file without write tracking must NOT be captured")
		}
	}
}

func TestSnapshotOutputsRecursiveScope(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, ".brine"))
	if err != nil {
		t.Fatal(err)
	}
	readSet := []string{"input.txt"}
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("in"), 0o644)
	builtAt := time.Now().UnixNano()
	artifacts := map[string]string{
		"pkg/mbedtls/lib/libmbedtls.a": "LIB",
		"pkg/mbedtls/include/x.h":      "H",
		"build/intermediate.o":         "OBJ",
	}
	for rel, content := range artifacts {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
		future := time.Now().Add(time.Second)
		os.Chtimes(p, future, future)
	}
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, []string{"pkg/**"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 2 {
		t.Fatalf("pkg/** must capture both nested pkg files, not the .o: got %+v", m.Blobs)
	}
	for _, b := range m.Blobs {
		if b.Path == "build/intermediate.o" {
			t.Fatalf("out-of-scope file captured: %s", b.Path)
		}
	}
}
