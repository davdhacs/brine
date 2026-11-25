package gocache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Filesystem backend: the same content-addressed store semantics as the
// Azure/GHA backends, on a local directory. For machines whose network
// path to a blob store is slower than their disk (measured live: a home
// server pulling 2.6 MB/s single-stream from Azure made receipt restores
// network-bound at 65s for 465 MB that disk serves in ~1s), and for
// LAN-shared caches (NFS, a mounted volume).
//
// BRINE_STORE_BACKEND=fs selects it; BRINE_FS_STORE_DIR sets the root
// (default ~/.cache/brine-store). Keys keep their slash-separated form
// (store/b/<hash>) as subdirectories.

// fsPath maps a store key to a path under the backend root. Keys are
// internal (fixed prefixes + content hashes) but guard against
// traversal anyway — a corrupted manifest must not make brine write
// outside the store root.
func (r *Remote) fsPath(key string) (string, error) {
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("fs backend: invalid key %q", key)
	}
	return filepath.Join(r.fsDir, filepath.FromSlash(key)), nil
}

// fsGet returns the blob at key, or nil,nil when absent. Raw bytes at
// rest (no gzip — disk is cheap; maybeGunzip in Get sniffs the magic
// bytes so raw content passes through unchanged).
func (r *Remote) fsGet(key string) ([]byte, error) {
	p, err := r.fsPath(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// fsPut writes body at key, last-write-wins. Atomic (temp + rename) so
// a concurrent reader never sees a partial blob.
func (r *Remote) fsPut(key string, body []byte) error {
	p, err := r.fsPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, p)
}

// fsPutIfNotExist writes only when absent; returns false if it exists.
// O_EXCL gives kernel-enforced first-write-wins.
func (r *Remote) fsPutIfNotExist(key string, body []byte) (bool, error) {
	p, err := r.fsPath(key)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(p)
		return false, err
	}
	return true, f.Close()
}

func (r *Remote) fsExists(key string) (bool, error) {
	p, err := r.fsPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// fsStreamToDest streams the blob at key to dest with sha256
// verification, temp+rename — the same semantics as streamURLToDest
// (unique tmp, hash checked before the rename, no partial outputs).
func (r *Remote) fsStreamToDest(key, expectHash, dest string) error {
	p, err := r.fsPath(key)
	if err != nil {
		return err
	}
	src, err := os.Open(p)
	if os.IsNotExist(err) {
		return fmt.Errorf("remote GET %s: not found", key)
	}
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".fetch-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), src)
	tmp.Close()
	if copyErr != nil {
		os.Remove(name)
		return copyErr
	}
	if hex.EncodeToString(h.Sum(nil)) != expectHash {
		os.Remove(name)
		return fmt.Errorf("integrity: %s content does not hash to key", key)
	}
	return os.Rename(name, dest)
}

// fsValidateAuth checks the root exists and is writable.
func (r *Remote) fsValidateAuth() error {
	if err := os.MkdirAll(r.fsDir, 0o755); err != nil {
		return fmt.Errorf("fs backend: cannot create %s: %w", r.fsDir, err)
	}
	probe, err := os.CreateTemp(r.fsDir, ".probe-*")
	if err != nil {
		return fmt.Errorf("fs backend: %s not writable: %w", r.fsDir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}
