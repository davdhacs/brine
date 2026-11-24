package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SnapshotOutputs stores ONLY the files a build produced: files that
// (a) are not in the input read-set, and (b) were created or modified
// after builtAt OR observed written by the write-tracker. The write
// set matters because installers PRESERVE SOURCE MTIMES (cmake
// --install copies headers with the original 2024 tarball timestamps)
// — mtime alone silently drops them from the receipt (found live:
// the dep receipt restored libs but no headers, and the dependent
// build failed on the missing include).
//
// This is the correctness-critical distinction from SnapshotDir (which
// captured the whole tree and, on a warm hit, overwrote non-input
// edits — see docs/sensors.md "Correctness rules").
//
// outputPatterns scopes the snapshot to deliverables when set (the
// collector: 898 build artifacts → 2 binaries; 1.6GB → 85MB shared).
// Semantics match restore: substring on the relative path, or
// filepath.Match glob when the pattern contains * ? [. The default
// (nil) keeps every output — full incremental-rebuild state.
func (s *Store) SnapshotOutputs(key, dir string, readSet []string, builtAt int64, outputPatterns []string, written map[string]bool) (*Manifest, error) {
	inputs := make(map[string]bool, len(readSet))
	for _, f := range readSet {
		inputs[filepath.ToSlash(f)] = true
	}

	m := &Manifest{
		Key:           key,
		Created:       time.Now(),
		OutputPatterns: outputPatterns,
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Hidden dirs are pruned (VCS, caches) EXCEPT when the
			// build itself created them (mtime > builtAt): build
			// systems keep validity state in dot-dirs (cargo's
			// target/debug/.fingerprint) that a receipt must restore
			// for the tool to accept the outputs. A "/**" scope
			// pattern covering the dir overrides the pruning: the
			// user explicitly wants everything beneath it (libtool's
			// bld/lib/.libs/ holds the real .so — restored with old
			// mtimes by the incremental path, pruned from the next
			// receipt, the chain loses the library — found live).
			if strings.HasPrefix(info.Name(), ".") && info.ModTime().UnixNano() <= builtAt {
				relD, _ := filepath.Rel(dir, path)
				relD = filepath.ToSlash(relD)
				if !scopeCoversDir(outputPatterns, relD) {
					return filepath.SkipDir
				}
			}
			return nil
		}

		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)

		// Never snapshot cache/VCS internals.
		if strings.HasPrefix(rel, ".brine") || strings.HasPrefix(rel, ".git/") {
			return nil
		}
		// Dot-FILES (e.g. .DS_Store) are skipped individually; dot-DIRECTORIES
		// (.git, .idea) are pruned. SkipDir on a file would skip the rest of
		// its directory — .DS_Store in the repo root would silently end the
		// walk and capture nothing.
		// Hidden FILES are skipped (state files are handled at the
		// directory level below).
		if strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}

		// Rule (a): inputs are never outputs — EXCEPT files written
		// during the build (mtime > builtAt): a build-writer cannot be
		// an input to the same build (make's freshness opens put
		// outputs in observed read-sets; flat projects).
		if inputs[rel] && info.ModTime().UnixNano() <= builtAt {
			return nil
		}
		// Rule (b): only files the build touched — by mtime OR by the
		// write-tracker. Installers that preserve source mtimes (cmake
		// --install) otherwise drop copied files (headers!) from the
		// receipt while capturing the freshly-compiled libs.
		if info.ModTime().UnixNano() <= builtAt && !written[rel] {
			return nil
		}
		// Scoped snapshot: only deliverables matching the patterns.
		// Filter BEFORE PutBlob — intermediates are never hashed,
		// stored locally, or uploaded. Exact path or glob (NOT
		// substring: "build/app" must not pull in build/app.o —
		// scope declarations are precise; substring stays a
		// restore-time convenience).
		if len(outputPatterns) > 0 && !matchOutputScope(outputPatterns, rel) {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			if os.Getenv("BRINE_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[dbg] open failed %s: %v\n", rel, err)
			}
			return nil
		}
		defer f.Close()
		hash, size, err := s.PutBlob(f)
		if err != nil {
			return nil
		}
		mode := uint32(info.Mode())
		if mode == 0o644 {
			mode = 0 // common case: omit
		}
		m.Blobs = append(m.Blobs, BlobRef{Hash: hash, Path: rel, Size: size, Mode: mode})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Record directories the build CREATED (mtime > builtAt), even
	// empty ones: make-style builds mkdir output dirs (collector's
	// container/libs) that hold no files — invisible to the blob set
	// but their absence must not invalidate input-dir listings.
	dirs := map[string]bool{}
	err2 := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") && info.ModTime().UnixNano() <= builtAt {
			return filepath.SkipDir
		}
		if info.ModTime().UnixNano() > builtAt {
			dirs[rel] = true
		}
		return nil
	})
	if err2 == nil {
		for d := range dirs {
			m.OutputDirs = append(m.OutputDirs, d)
		}
		sort.Strings(m.OutputDirs)
	}

	sort.Slice(m.Blobs, func(i, j int) bool { return m.Blobs[i].Path < m.Blobs[j].Path })
	return m, nil
}

// scopeCoversDir reports whether any "/**" output pattern's prefix
// covers the directory (the user explicitly scoped into it — hidden
// or not).
func scopeCoversDir(patterns []string, relDir string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/**") {
			prefix := strings.TrimSuffix(p, "**")
			if strings.HasPrefix(relDir+"/", prefix) {
				return true
			}
		}
	}
	return false
}

// matchOutputScope matches a snapshot-scope pattern: exact relative
// path, filepath.Match glob when the pattern contains * ? [ — with
// ** supported as a recursive wildcard (matches across separators:
// "pkg/**" = everything under pkg/) — or a directory prefix when the
// pattern ends in /** .
func matchOutputScope(patterns []string, rel string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/**") {
			// recursive: everything under the prefix
			prefix := strings.TrimSuffix(p, "**")
			if strings.HasPrefix(rel, prefix) {
				return true
			}
			continue
		}
		if strings.Contains(p, "**") {
			// ** in the middle: fall back to segment-wise ** = * per
			// filepath.Match, then prefix-match the leading part
			if globMatchRecursive(p, rel) {
				return true
			}
			continue
		}
		if strings.ContainsAny(p, "*?[") {
			if ok, _ := filepath.Match(p, rel); ok {
				return true
			}
			if ok, _ := filepath.Match(p, filepath.Base(rel)); ok {
				return true
			}
		} else if rel == p {
			return true
		}
	}
	return false
}

// globMatchRecursive matches a pattern containing ** anywhere, where
// ** crosses path separators. Implemented by expanding the pattern
// into a regexp over the slash-separated path.
func globMatchRecursive(pattern, rel string) bool {
	var rx strings.Builder
	rx.WriteString("^")
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			rx.WriteString("(?:[^/]+/)*") // zero or more segments
			continue
		}
		// translate single-segment glob (* ? [class]) to regex
		for _, r := range seg {
			switch r {
			case '*':
				rx.WriteString("[^/]*")
			case '?':
				rx.WriteString("[^/]")
			case '[':
				rx.WriteString("[")
			case ']':
				rx.WriteString("]")
			default:
				rx.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		rx.WriteString("/")
	}
	compiled, err := regexp.Compile(strings.TrimSuffix(rx.String(), "/") + "$")
	if err != nil {
		return false
	}
	return compiled.MatchString(rel)
}

// OutputHashes returns path → content hash for every output blob in a
// manifest — the "what did this build produce" ground truth that the
// learned model diffs against.
func (m *Manifest) OutputHashes() map[string]string {
	out := make(map[string]string, len(m.Blobs))
	for _, b := range m.Blobs {
		out[b.Path] = b.Hash
	}
	return out
}

// LatestManifestForCommand returns the best manifest for a command:
// richest first (most outputs), recency as tiebreak. NOT simply the
// latest — a no-op rebuild's 0-blob receipt is newer than the real
// build's and would shadow it forever (empty "hits").
func (s *Store) LatestManifestForCommand(command string) *Manifest {
	var best *Manifest
	for _, m := range s.ManifestsForCommand(command) {
		if best == nil ||
			len(m.Blobs) > len(best.Blobs) ||
			(len(m.Blobs) == len(best.Blobs) && m.Created.After(best.Created)) {
			best = m
		}
	}
	return best
}

// formatReasons renders validation reasons compactly for logs.
func FormatReasons(reasons []string) string {
	if len(reasons) > 3 {
		reasons = append(append([]string{}, reasons[:3]...), fmt.Sprintf("… +%d more", len(reasons)-3))
	}
	return strings.Join(reasons, ", ")
}

// LatestForClosure returns the most recent manifest with the given
// closure hash (excluding excludeKey), or nil. The nondeterminism
// detector's index: same closure + different key/output = divergence.
func (s *Store) LatestForClosure(closureHash, excludeKey string) *Manifest {
	var best *Manifest
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
		if json.Unmarshal(data, &m) == nil && m.ClosureHash == closureHash && m.Key != excludeKey {
			if best == nil || m.Created.After(best.Created) {
				best = &m
			}
		}
	}
	return best
}

// AllManifests returns all stored manifests as raw maps (for the
// cross-command dependency graph).
func (s *Store) AllManifests() []map[string]interface{} {
	var out []map[string]interface{}
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
		var m map[string]interface{}
		if json.Unmarshal(data, &m) == nil && m["command"] != nil {
			out = append(out, m)
		}
	}
	return out
}
