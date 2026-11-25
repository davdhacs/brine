package gocache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func newFSRemote(t *testing.T) *Remote {
	t.Helper()
	dir := t.TempDir()
	r := &Remote{backend: "fs", fsDir: dir}
	if err := r.ValidateAuth(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return r
}

func TestFSBackendRoundTrip(t *testing.T) {
	r := newFSRemote(t)
	key := "store/b/abc123"
	body := []byte("hello filesystem backend")

	// absent
	if got, err := r.Get(key); err != nil || got != nil {
		t.Fatalf("Get on absent key = (%v, %v), want (nil, nil)", got, err)
	}
	if ok, err := r.ExistsPub(key); err != nil || ok {
		t.Fatalf("Exists on absent = (%v, %v), want (false, nil)", ok, err)
	}

	// putIfNotExist creates
	created, err := r.PutIfNotExistPub(key, body)
	if err != nil || !created {
		t.Fatalf("PutIfNotExist create = (%v, %v), want (true, nil)", created, err)
	}
	// duplicate is a no-op
	created, err = r.PutIfNotExistPub(key, []byte("other"))
	if err != nil || created {
		t.Fatalf("PutIfNotExist dup = (%v, %v), want (false, nil)", created, err)
	}
	// the dup did NOT overwrite
	got, err := r.Get(key)
	if err != nil || string(got) != string(body) {
		t.Fatalf("Get after dup = (%q, %v)", got, err)
	}

	// put overwrites
	if err := r.PutPub(key, []byte("v2")); err != nil {
		t.Fatalf("PutPub: %v", err)
	}
	got, _ = r.Get(key)
	if string(got) != "v2" {
		t.Fatalf("Get after PutPub = %q, want v2", got)
	}

	// streaming with correct hash
	want := []byte("streamed content")
	sum := sha256.Sum256(want)
	hash := hex.EncodeToString(sum[:])
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := r.PutPub("store/b/"+hash, want); err != nil {
		t.Fatalf("PutPub stream: %v", err)
	}
	if err := r.GetStreamingRaw("store/b/"+hash, hash, dest); err != nil {
		t.Fatalf("GetStreamingRaw: %v", err)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != string(want) {
		t.Fatalf("streamed = %q", data)
	}

	// streaming with WRONG hash refuses and leaves no partial file
	bad := filepath.Join(t.TempDir(), "bad.bin")
	err = r.GetStreamingRaw("store/b/"+hash, "deadbeef", bad)
	if err == nil {
		t.Fatal("wrong hash must fail")
	}
	if _, statErr := os.Stat(bad); statErr == nil {
		t.Fatal("failed stream must not leave a partial file")
	}
}

func TestFSBackendKeyTraversal(t *testing.T) {
	r := newFSRemote(t)
	for _, key := range []string{"/etc/passwd", "a/../../b", ".."} {
		if _, err := r.Get(key); err == nil {
			t.Fatalf("Get(%q) must reject traversal keys", key)
		}
	}
}
