// Package store provides a content-addressed blob store with
// manifests for CI memoization.
//
// Architecture:
//
//	Blobs   — raw file content, stored by SHA-256. Deduplicated.
//	Manifests — structured records mapping a cache key to a set of
//	           blob references with their target paths.
//
// A manifest answers: "for this key, what files should exist, and
// where do I find their content?" Multiple manifests referencing the
// same blob share storage automatically.
package store

import (
	"github.com/davdhacs/brine/buildinfo"
	"github.com/davdhacs/brine/gocache"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ImageRef is a docker image output (receipt: image ID, restore: re-tag).
type ImageRef struct {
	ImageID string `json:"image_id"`
	Tag     string `json:"tag"`
}

// BlobRef is a reference to a stored blob.
type BlobRef struct {
	Hash string `json:"hash"`           // SHA-256 of content
	Path string `json:"path"`           // relative path for restoration
	Size int64  `json:"size"`           // content size in bytes
	Mode uint32 `json:"mode,omitempty"` // file mode (exec bit survives restore)
}

// Manifest maps a cache key to a set of output blobs.
type Manifest struct {
	Key      string    `json:"key"`
	Blobs    []BlobRef `json:"blobs"`
	Created  time.Time `json:"created"`
	LastRead time.Time `json:"last_read"`
	Command  string    `json:"command,omitempty"`

	// Provenance: how the read-set was discovered and how to re-derive
	// it. Strategy picks the revalidation tier (see provenance.go);
	// Closure re-derives the read-set without re-running the command.
	Strategy     string            `json:"strategy,omitempty"`
	ReadSet      []string          `json:"read_set,omitempty"`
	InputHashes  map[string]string `json:"input_hashes,omitempty"`
	DirListings  map[string]string `json:"dir_listings,omitempty"`
	Closure      string            `json:"closure,omitempty"`
	Toolchain    string            `json:"toolchain,omitempty"`
	BuiltAt      int64             `json:"built_at,omitempty"`
	ActionStats  *gocache.Stats    `json:"action_stats,omitempty"`
	BuildInfo    *buildinfo.Info   `json:"build_info,omitempty"`
	VCSCommit    string            `json:"vcs_commit,omitempty"`
	OutputDirs   []string          `json:"output_dirs,omitempty"`
	ImageOutputs []ImageRef        `json:"image_outputs,omitempty"`
	KeyKind      string            `json:"key_kind,omitempty"` // "content" (default) | "commit"
	ClosureHash  string            `json:"closure_hash,omitempty"`
	KeyTail      string            `json:"key_tail,omitempty"`
	OutputPatterns []string        `json:"output_patterns,omitempty"` // snapshot scope; nil = all outputs
	Bundles        []BundleRef    `json:"bundles,omitempty"`         // small-blob transport bundles (backend-agnostic)
}

// Store is the content-addressed blob store with manifests.
type Store struct {
	root   string // directory root
	remote *gocache.Remote

	// single-flight for remote fetches: concurrent GetBlob calls for
	// the same hash collapse to ONE download — waiters block on the
	// channel until the fetch completes (found via the symlink-race
	// investigation: correct-but-wasteful double downloads)
	fetchMu  sync.Mutex
	inFlight map[string]chan struct{}
}

// New creates a Store rooted at dir.
func New(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "manifests")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("store init %s: %w", d, err)
		}
	}
	return &Store{root: dir, inFlight: map[string]chan struct{}{}}, nil
}

// blobPath returns the on-disk path for a blob hash.
// Blobs are sharded by first 2 hex chars to avoid huge directories.
func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.root, "blobs", hash[:2], hash)
}

// manifestPath returns the on-disk path for a manifest key.
func (s *Store) manifestPath(key string) string {
	return filepath.Join(s.root, "manifests", key+".json")
}

// PutBlob stores content and returns its hash.
func (s *Store) PutBlob(content io.Reader) (string, int64, error) {
	// Read content to compute hash
	data, err := io.ReadAll(content)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	p := s.blobPath(hash)
	if _, err := os.Stat(p); err == nil {
		// Already exists — dedup hit
		return hash, int64(len(data)), nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", 0, err
	}
	return hash, int64(len(data)), nil
}

// GetBlob returns content by hash, fetching from the remote store
// (with integrity verification) when not local.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	data, err := os.ReadFile(s.blobPath(hash))
	if os.IsNotExist(err) && s.hasRemote() {
		if ferr := s.remoteFetchBlob(hash); ferr == nil {
			data, err = os.ReadFile(s.blobPath(hash))
		} else {
			// the real failure (timeout, integrity, HTTP) — never mask
			// a fetch error as absence
			return nil, ferr
		}
	}
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("blob not found: %s", hash)
	}
	return data, err
}

// writeAtomic + markBlob: small helpers for the remote tier.
func writeAtomic(path string, data []byte) error {
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) markBlob(hash string) {}

// PutManifest stores a manifest (and updates LastRead). With a remote
// attached, also uploads the manifest and its blobs — this is what
// makes receipts cross-machine.
func (s *Store) PutManifest(m *Manifest) error {
	// bundles must be prepared BEFORE marshaling: both the local copy
	// and the remote upload carry the index
	if err := s.prepareBundles(m); err != nil {
		return fmt.Errorf("bundle prep: %w", err)
	}
	m.LastRead = time.Now()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.manifestPath(m.Key), data, 0o644); err != nil {
		return err
	}
	if s.hasRemote() {
		if err := s.remoteUploadManifest(m); err != nil {
			// Remote failure must not fail the build — the local
			// receipt is still valid on this machine — but never
			// silently: an expired token or dead store otherwise looks
			// like cache misses forever.
			fmt.Fprintf(os.Stderr, "[brine] remote receipt upload failed: %v (local receipt still valid)\n", err)
			return nil
		}
	}
	return nil
}

// GetManifest retrieves a manifest by key, updating LastRead. Falls
// back to the remote store when the local copy is absent.
func (s *Store) GetManifest(key string) (*Manifest, error) {
	data, err := os.ReadFile(s.manifestPath(key))
	if os.IsNotExist(err) && s.hasRemote() {
		if m, rerr := s.remoteFetchManifest(key); rerr == nil && m != nil {
			// Cache it locally for subsequent reads.
			if jdata, jerr := json.MarshalIndent(m, "", "  "); jerr == nil {
				os.WriteFile(s.manifestPath(key), jdata, 0o644)
			}
			return m, nil
		}
	}
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("manifest not found: %s", key)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest parse %s: %w", key, err)
	}
	m.LastRead = time.Now()
	_ = os.WriteFile(s.manifestPath(key), data, 0o644) // update atime via rewrite
	return &m, nil
}

// HasManifest reports whether a manifest exists (locally or remote).
func (s *Store) HasManifest(key string) bool {
	if _, err := os.Stat(s.manifestPath(key)); err == nil {
		return true
	}
	if s.hasRemote() {
		ok, err := s.remote.ExistsPub(remoteManifestPrefix + key)
		return err == nil && ok
	}
	return false
}

// RestoreManifest restores all blobs referenced by a manifest into dir.
// Returns the number of files restored and total bytes.
//
// CERTAINTY: every blob is re-verified (sha256 == its key) before it
// is written, INCLUDING locally-stored ones — a corrupted local blob
// file would otherwise serve wrong bytes silently. Opt out only with
// BRINE_TRUST_LOCAL=1 (skips ~1s per GB on restore).
//
// Downloads run CONCURRENTLY (bounded 16): a 125-blob dep receipt
// restored sequentially measured ~35s (one remote fetch per small
// header) — slower than the incumbent's single-tarball restore. The
// per-blob sha256 verification is unchanged; any integrity failure
// aborts with the first error (visible by design).
func (s *Store) RestoreManifest(m *Manifest, dir string) (int, int64, error) {
	// bundles first: one fetch per bundle instead of one per member
	if err := s.fetchBundles(m); err != nil {
		return 0, 0, err
	}
	trustLocal := os.Getenv("BRINE_TRUST_LOCAL") == "1"
	var mu sync.Mutex
	var count int
	var total int64
	// Dedup by content hash: outputs with identical content (symlinked
	// or copied artifacts — libcurl.so.4 vs libcurl.so.4.8.0) share a
	// hash; fetching the same blob twice in parallel races the shared
	// .tmp path (found live: "rename .tmp: no such file").
	unique := map[string][]BlobRef{}
	for _, ref := range m.Blobs {
		unique[ref.Hash] = append(unique[ref.Hash], ref)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	errCh := make(chan error, len(unique))
	for _, refs := range unique {
		ref := refs[0] // fetch once; write to every path below
		wg.Add(1)
		sem <- struct{}{}
		go func(ref BlobRef, paths []BlobRef) {
			defer wg.Done()
			defer func() { <-sem }()
			data, err := s.GetBlob(ref.Hash)
			if err != nil {
				errCh <- fmt.Errorf("restore %s (blob %s): %w", ref.Path, ref.Hash[:12], err)
				return
			}
			if !trustLocal {
				if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != ref.Hash {
					errCh <- fmt.Errorf(
						"restore %s: INTEGRITY FAILURE blob %s: content hash mismatch — refusing to write",
						ref.Path, ref.Hash[:12])
					return
				}
			}
			for _, p := range paths {
				target := filepath.Join(dir, p.Path)
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					errCh <- err
					return
				}
				if err := os.WriteFile(target, data, 0o644); err != nil {
					errCh <- err
					return
				}
				// Restore with the BUILD-time mtime: build systems with
				// mtime-based freshness (cargo's fingerprints) treat
				// restore-time mtimes as "newer than sources" and recompile
				// (measured: 15 of ~170 crates on the fact case study).
				if m.BuiltAt > 0 {
					t := time.Unix(0, m.BuiltAt)
					os.Chtimes(target, t, t)
				}
				if p.Mode != 0 {
					os.Chmod(target, os.FileMode(p.Mode))
				}
				mu.Lock()
				count++
				mu.Unlock()
			}
			mu.Lock()
			total += int64(len(data)) * int64(len(paths))
			mu.Unlock()
		}(ref, refs)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return count, total, err
	}
	return count, total, nil
}

// matchAnyPattern applies the shared pattern semantics: filepath.Match
// (glob) when the pattern contains glob characters (* ? [), substring
// match otherwise — matching against both the relative path and its
// base name.
func matchAnyPattern(patterns []string, rel string) bool {
	for _, p := range patterns {
		if strings.ContainsAny(p, "*?[") {
			if ok, _ := filepath.Match(p, rel); ok {
				return true
			}
			if ok, _ := filepath.Match(p, filepath.Base(rel)); ok {
				return true
			}
		} else if strings.Contains(rel, p) {
			return true
		}
	}
	return false
}

// RestoreManifestMatching restores only blobs whose manifest-relative
// path matches the pattern: filepath.Match (glob) when the pattern
// contains glob characters (* ? [), substring match otherwise —
// `brine restore <key> collector` fetches every output with
// "collector" in its path without remembering the full layout.
func (s *Store) RestoreManifestMatching(m *Manifest, dir, pattern string) (int, int64, error) {
	// carry the bundle index: members of filtered blobs still arrive
	// via their bundle (one fetch), then the pattern selects files
	sub := &Manifest{Key: m.Key, BuiltAt: m.BuiltAt, Bundles: m.Bundles}
	for _, ref := range m.Blobs {
		if matchAnyPattern([]string{pattern}, ref.Path) {
			sub.Blobs = append(sub.Blobs, ref)
		}
	}
	if len(sub.Blobs) == 0 {
		return 0, 0, fmt.Errorf("no outputs match %q (manifest has %d outputs)", pattern, len(m.Blobs))
	}
	return s.RestoreManifest(sub, dir)
}

// SnapshotDir walks dir and stores every file as a blob, returning
// a Manifest with the results. Files matching exclude patterns are
// skipped.
func (s *Store) SnapshotDir(key, dir string, exclude []string) (*Manifest, error) {
	m := &Manifest{
		Key:     key,
		Created: time.Now(),
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		base := filepath.Base(rel)

		// Always skip hidden files and VCS dirs
		if strings.HasPrefix(base, ".") {
			return nil
		}
		parts := strings.Split(rel, "/")
		for _, part := range parts[:len(parts)-1] {
			if strings.HasPrefix(part, ".") {
				return filepath.SkipDir
			}
		}

		// Check exclusions
		for _, ex := range exclude {
			if matched, _ := filepath.Match(ex, rel); matched {
				return nil
			}
			if matched, _ := filepath.Match(ex, filepath.Base(rel)); matched {
				return nil
			}
			// Also check if any parent dir matches
			for _, part := range strings.Split(rel, "/") {
				if matched, _ := filepath.Match(ex, part); matched {
					return nil
				}
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return nil // skip unreadable
		}
		defer f.Close()

		hash, size, err := s.PutBlob(f)
		if err != nil {
			return nil // skip on error
		}

		m.Blobs = append(m.Blobs, BlobRef{
			Hash: hash,
			Path: rel,
			Size: size,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort for deterministic manifests
	sort.Slice(m.Blobs, func(i, j int) bool {
		return m.Blobs[i].Path < m.Blobs[j].Path
	})

	return m, nil
}

// OutputPaths returns all file paths that appear in any manifest,
// representing the union of all cached output files. Callers use this
// to exclude previous outputs from key computation, making keys stable
// across repeated runs of the same command.
func (s *Store) OutputPaths() []string {
	var paths []string
	manifestDir := filepath.Join(s.root, "manifests")
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(data, &m) == nil {
			for _, ref := range m.Blobs {
				paths = append(paths, ref.Path)
			}
		}
	}
	return paths
}

// ManifestsForCommand returns all manifests that were created by the
// given command, sorted by creation time (oldest first).
func (s *Store) ManifestsForCommand(command string) []*Manifest {
	var out []*Manifest
	manifestDir := filepath.Join(s.root, "manifests")
	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestDir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(data, &m) == nil && m.Command == command {
			out = append(out, &m)
		}
	}
	return out
}

// GC removes blobs and manifests not accessed within maxAge.
// Returns bytes freed and count of items removed.
func (s *Store) GC(maxAge time.Duration) (int64, int, error) {
	cutoff := time.Now().Add(-maxAge)
	var freed int64
	var removed int

	// GC manifests
	manifestDir := filepath.Join(s.root, "manifests")
	entries, _ := os.ReadDir(manifestDir)
	var liveHashes = make(map[string]bool)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(manifestDir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(p)
			removed++
		} else {
			// Read manifest to find live blobs
			data, _ := os.ReadFile(p)
			var m Manifest
			if json.Unmarshal(data, &m) == nil {
				for _, ref := range m.Blobs {
					liveHashes[ref.Hash] = true
				}
				// bundles and their members stay reachable with the
				// manifest that references them
				for _, b := range m.Bundles {
					liveHashes[b.Hash] = true
					for _, h := range b.Members {
						liveHashes[h] = true
					}
				}
			}
		}
	}

	// GC blobs (remove those not referenced by any live manifest)
	blobDir := filepath.Join(s.root, "blobs")
	filepath.Walk(blobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		hash := filepath.Base(path)
		if !liveHashes[hash] {
			os.Remove(path)
			freed += info.Size()
			removed++
		}
		return nil
	})

	return freed, removed, nil
}
