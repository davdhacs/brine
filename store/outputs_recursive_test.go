package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	m, err := s.SnapshotOutputs("k", dir, readSet, builtAt, []string{"pkg/**"})
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
