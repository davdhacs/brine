package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBundlePackDeterministic(t *testing.T) {
	members := map[string][]byte{
		"aa" + string(bytes.Repeat([]byte{1}, 30)): []byte("alpha"),
		"bb" + string(bytes.Repeat([]byte{2}, 30)): []byte("beta"),
	}
	p1, h1 := packBundle(members)
	p2, h2 := packBundle(members)
	if h1 != h2 || !bytes.Equal(p1, p2) {
		t.Fatalf("pack must be deterministic: %s vs %s", h1[:12], h2[:12])
	}
	// different member ORDER in the map must not matter (sorted pack)
	m3 := map[string][]byte{}
	for k, v := range members {
		m3[k] = v
	}
	_, h3 := packBundle(m3)
	if h3 != h1 {
		t.Fatalf("insertion order must not affect bundle hash")
	}
}

func TestBundleUnpackRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(filepath.Join(dir, ".brine"))
	members := map[string][]byte{}
	for i := 0; i < 5; i++ {
		data := []byte{byte('a' + i)}
		sum := sha256Sum(data)
		members[sum] = data
		if _, _, err := s.PutBlob(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	packed, hash := packBundle(members)
	if _, _, err := s.PutBlob(bytes.NewReader(packed)); err != nil {
		t.Fatal(err)
	}
	// delete the member blobs locally
	for h := range members {
		os.Remove(s.blobPath(h))
	}
	b := BundleRef{Hash: hash, Size: int64(len(packed)), Members: keysOf(members)}
	if err := s.unpackBundle(b); err != nil {
		t.Fatal(err)
	}
	for h, want := range members {
		got, err := os.ReadFile(s.blobPath(h))
		if err != nil {
			t.Fatalf("member %s not restored from bundle: %v", h[:12], err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("member %s content mismatch", h[:12])
		}
	}
}

func TestBundleUnpackIntegrity(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(filepath.Join(dir, ".brine"))
	// hand-craft a bundle whose member data does NOT hash to its key
	var buf bytes.Buffer
	buf.WriteString(bundleMagic)
	buf.Write([]byte{1, 0, 0, 0}) // count = 1
	buf.Write(bytes.Repeat([]byte{0xAB}, 32)) // bogus hash
	buf.Write([]byte{4, 0, 0, 0, 0, 0, 0, 0}) // size = 4
	buf.Write([]byte("evil"))                 // content != hash
	packed := buf.Bytes()
	hash := sha256Sum(packed)
	if _, _, err := s.PutBlob(bytes.NewReader(packed)); err != nil {
		t.Fatal(err)
	}
	err := s.unpackBundle(BundleRef{Hash: hash, Size: int64(len(packed)), Members: []string{"bogus"}})
	if err == nil {
		t.Fatalf("member hash mismatch must fail loudly")
	}
}

func TestPrepareBundlesIntegration(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(filepath.Join(dir, ".brine"))
	// build outputs: 20 small files + 1 large
	readSet := []string{"input.txt"}
	os.WriteFile(filepath.Join(dir, "input.txt"), []byte("in"), 0o644)
	builtAt := time.Now().UnixNano()
	var artifacts map[string]string
	artifacts = map[string]string{}
	for i := 0; i < 20; i++ {
		artifacts[filepath.ToSlash(filepath.Join("pkg", "inc", string(rune('a'+i))+".h"))] = "header-" + string(rune('a'+i))
	}
	artifacts["pkg/lib/libbig.a"] = string(bytes.Repeat([]byte{0}, 2<<20)) // 2MB > threshold
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
	if len(m.Blobs) != 21 {
		t.Fatalf("expected 21 outputs, got %d", len(m.Blobs))
	}
	if err := s.prepareBundles(m); err != nil {
		t.Fatal(err)
	}
	if len(m.Bundles) != 1 {
		t.Fatalf("expected 1 bundle, got %d", len(m.Bundles))
	}
	b := m.Bundles[0]
	if len(b.Members) != 20 {
		t.Fatalf("bundle must contain the 20 small files, got %d", len(b.Members))
	}
	// the large file must NOT be bundled
	for _, h := range b.Members {
		if h == hashOf([]byte(artifacts["pkg/lib/libbig.a"])) {
			t.Fatalf("large file must stay an individual blob")
		}
	}
	// transport refs: bundle + large only = 2, not 21
	refs := transportRefs(m)
	if len(refs) != 2 {
		t.Fatalf("transportRefs must be [bundle, large], got %d", len(refs))
	}
	// delete members locally; unpack restores them
	for _, h := range b.Members {
		os.Remove(s.blobPath(h))
	}
	if err := s.unpackBundle(b); err != nil {
		t.Fatal(err)
	}
	for _, h := range b.Members {
		if _, err := os.Stat(s.blobPath(h)); err != nil {
			t.Fatalf("member %s not restored", h[:12])
		}
	}
}
