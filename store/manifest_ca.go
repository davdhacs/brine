// Content-addressed manifest storage: manifests stored at
// `manifests/<receipt_key>/<manifest_hash>.json` where manifest_hash
// is sha256 of the manifest's own JSON content. This makes manifest
// overwrites structurally impossible — different content → different
// hash → different path.
//
// On read: list all candidates under the receipt key. One candidate
// is normal. Multiple candidates = same inputs, different outputs =
// divergence (non-determinism or attack) → alert, don't silently pick.
//
// Local disk writes use O_EXCL (kernel-enforced first-write-wins).
// Remote writes use If-None-Match (server-enforced for honest clients).
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// manifestSelfHash computes the hash of the manifest's JSON content.
// Used as the storage filename, making the path content-addressed.
func manifestSelfHash(m *Manifest) string {
	// Hash only the stable fields — LastRead changes on every access
	// and must not affect the content-addressed path.
	stable := *m
	stable.LastRead = time.Time{}
	data, _ := json.Marshal(&stable)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PutManifestCA stores a manifest at the content-addressed path:
// manifests/<key>/<manifest_hash>.json
// Uses O_EXCL (kernel-enforced first-write-wins on local disk).
func (s *Store) PutManifestCA(m *Manifest) error {
	// Compute self-hash BEFORE setting LastRead (which changes on each
	// call — the content-addressed path must be based on the stable
	// content, not the access timestamp).
	selfHash := manifestSelfHash(m)

	m.LastRead = time.Now()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Join(s.root, "manifests", m.Key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("manifest dir: %w", err)
	}

	path := filepath.Join(dir, selfHash+".json")

	// O_EXCL: fails if file already exists (kernel-enforced).
	// If it exists with the same content (same hash), that's fine —
	// it was already cached by a previous build.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// Same content, same path — already cached. Not an error.
			return nil
		}
		return fmt.Errorf("manifest write: %w", err)
	}
	defer f.Close()
	_, err = f.Write(data)

	// Remote upload: If-None-Match (server-enforced for honest clients)
	if err == nil && s.hasRemote() {
		if rerr := s.remoteUploadManifest(m); rerr != nil {
			fmt.Fprintf(os.Stderr, "[brine] remote manifest upload: %v\n", rerr)
			// Local write succeeded; remote failure doesn't block
		}
	}

	return err
}

// GetManifestCA retrieves manifest candidates for a receipt key.
// Returns the single candidate, or an error if:
//   - no candidates exist (cache miss)
//   - multiple candidates exist (divergence — same inputs, different
//     outputs; either non-determinism or a tampering attack)
func (s *Store) GetManifestCA(key string) (*Manifest, error) {
	dir := filepath.Join(s.root, "manifests", key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no manifest for key %s", key)
		}
		return nil, err
	}

	var candidates []*Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(data, &m) == nil {
			candidates = append(candidates, &m)
		}
	}

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("no manifest for key %s", key)
	case 1:
		// Single candidate — normal case
		return candidates[0], nil
	default:
		// Multiple candidates — same inputs produced different outputs.
		// This is either non-determinism or a delete+replace attack.
		// Report it; don't silently pick one.
		var hashes []string
		for _, c := range candidates {
			hashes = append(hashes, manifestSelfHash(c)[:12])
		}
		sort.Strings(hashes)
		return nil, fmt.Errorf(
			"DIVERGENCE: %d manifest candidates for key %s (hashes: %v) — "+
				"same inputs, different outputs. Run `brine audit` to investigate",
			len(candidates), key, hashes)
	}
}

// HasManifestCA reports whether any candidate exists for a key.
func (s *Store) HasManifestCA(key string) bool {
	dir := filepath.Join(s.root, "manifests", key)
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// ListManifestHashes returns all manifest content-hashes for a key.
// Useful for diagnostics and the divergence report.
func (s *Store) ListManifestHashes(key string) []string {
	dir := filepath.Join(s.root, "manifests", key)
	entries, _ := os.ReadDir(dir)
	var hashes []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			hashes = append(hashes, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(hashes)
	return hashes
}
