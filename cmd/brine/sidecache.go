// Side caches: lockfile-keyed directory snapshots for dependency
// caches that are expensive to populate but cheap to copy. This is
// the CA branch's vendor/UI caching pattern, generalized.
//
// A side cache is a directory whose CONTENT is determined by a
// lockfile (or any small key file) — same lockfile = same directory
// content. Examples:
//
//	node_modules/  ← keyed by package-lock.json
//	vendor/        ← keyed by go.mod + go.sum
//	~/.cache/pip   ← keyed by requirements.txt
//
// The cache stores a tar of the directory, content-addressed by the
// lockfile hash. On a fresh runner, the directory is restored from
// the blob store instead of being repopulated by the package manager.
//
// This is distinct from receipts (which memoize the BUILD command's
// output). Side caches memoize the DEPENDENCY RESOLUTION step.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SideCache describes a dependency cache to snapshot.
type SideCache struct {
	// Dir is the directory to cache (relative to the project root).
	Dir string
	// KeyFiles are the files whose content determines the directory
	// content (lockfiles, manifests).
	KeyFiles []string
	// RestoreCmd is the command to run if the cache is not available
	// (e.g., "npm install", "go mod download"). Empty = no auto-restore.
	RestoreCmd string
}

// DefaultSideCaches are the common patterns, auto-detected.
func defaultSideCaches(wd string) []SideCache {
	var caches []SideCache

	// npm/node_modules
	if _, err := os.Stat(filepath.Join(wd, "package-lock.json")); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "node_modules")); err == nil {
			caches = append(caches, SideCache{
				Dir:        "node_modules",
				KeyFiles:   []string{"package-lock.json"},
				RestoreCmd: "npm install",
			})
		}
	}

	// Go vendor/
	if _, err := os.Stat(filepath.Join(wd, "vendor")); err == nil {
		keyFiles := []string{"go.mod"}
		if _, err := os.Stat(filepath.Join(wd, "go.sum")); err == nil {
			keyFiles = append(keyFiles, "go.sum")
		}
		caches = append(caches, SideCache{
			Dir:        "vendor",
			KeyFiles:   keyFiles,
			RestoreCmd: "go mod vendor",
		})
	}

	return caches
}

// sideCacheKey computes the content-addressed key for a side cache.
func sideCacheKey(wd string, sc SideCache) string {
	h := sha256.New()
	for _, kf := range sc.KeyFiles {
		data, err := os.ReadFile(filepath.Join(wd, kf))
		if err != nil {
			continue
		}
		h.Write(data)
	}
	return "sidecache/" + sc.Dir + "/" + hex.EncodeToString(h.Sum(nil))[:16]
}

// saveSideCache uploads a directory as a tar to the blob store.
type sideCacheStore interface {
	PutIfNotExistPub(key string, body []byte) (bool, error)
	Get(key string) ([]byte, error)
}

func saveSideCache(wd string, sc SideCache, st sideCacheStore) error {
	if st == nil {
		return nil
	}

	key := sideCacheKey(wd, sc)
	dirPath := filepath.Join(wd, sc.Dir)

	// Create tar.gz in memory (streaming for large dirs would be better)
	tarData, err := tarDirectory(dirPath, wd)
	if err != nil {
		return fmt.Errorf("tar %s: %w", sc.Dir, err)
	}

	if _, err := st.PutIfNotExistPub(key, tarData); err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}

	fmt.Printf("[brine] side-cache: %s saved (%.1f MB, key %s)\n",
		sc.Dir, float64(len(tarData))/1024/1024, key[:24])
	return nil
}

// restoreSideCache downloads and extracts a side cache from the blob store.
func restoreSideCache(wd string, sc SideCache, st sideCacheStore) (bool, error) {
	if st == nil {
		return false, nil
	}

	key := sideCacheKey(wd, sc)
	data, err := st.Get(key)
	if err != nil || data == nil {
		return false, nil // not in cache
	}

	// Extract the tar
	if err := extractTar(data, wd); err != nil {
		return false, fmt.Errorf("extract %s: %w", sc.Dir, err)
	}

	fmt.Printf("[brine] side-cache: %s restored (%.1f MB)\n",
		sc.Dir, float64(len(data))/1024/1024)
	return true, nil
}

// tarDirectory creates a gzipped tar of a directory, relative to base.
func tarDirectory(dirPath, base string) ([]byte, error) {
	// This is a simplified version — for large directories, streaming
	// to a temp file would be better (see gocache/stream.go for the
	// streaming pattern)
	var files []string
	filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(base, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})

	// Use tar + gzip
	var tarBuffer = &bytesBuffer{}
	gzw := gzip.NewWriter(tarBuffer)
	tw := tar.NewWriter(gzw)

	for _, rel := range files {
		fullPath := filepath.Join(base, rel)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}

		hdr := &tar.Header{
			Name: rel,
			Mode: int64(info.Mode().Perm()),
			Size: info.Size(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		f, err := os.Open(fullPath)
		if err != nil {
			return nil, err
		}
		io.Copy(tw, f)
		f.Close()
	}

	tw.Close()
	gzw.Close()
	return tarBuffer.data, nil
}

// extractTar extracts a tar.gz into base.
func extractTar(data []byte, base string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		// Maybe not gzipped — try raw tar
		gzr = nil
	}

	var tr *tar.Reader
	if gzr != nil {
		tr = tar.NewReader(gzr)
	} else {
		tr = tar.NewReader(bytes.NewReader(data))
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(base, hdr.Name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		f, err := os.Create(target)
		if err != nil {
			return err
		}
		io.Copy(f, tr)
		f.Close()
	}
	return nil
}

// bytesBuffer is a simple in-memory buffer.
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
