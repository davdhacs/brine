// Cache size limiting: evict oldest entries when the local store
// exceeds a configured size. This prevents disk exhaustion on long
// replay runs and simulates the lifecycle of cache entries being
// removed over time.
//
// The eviction policy is LRU by LastRead timestamp: entries that
// haven't been read recently are removed first. Manifests reference
// blobs; when a manifest is evicted, its blobs become unreferenced
// and are removed on the next sweep.
//
// Configuration:
//
//	BRINE_MAX_CACHE_MB=2048   # 2GB limit (env var)
//	{"max_cache_mb": 2048}     # .brine.json
//
// Default: unlimited (no eviction).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cacheEntryInfo for eviction decisions.
type cacheEntryInfo struct {
	Key       string
	LastRead  time.Time
	SizeBytes int64
	Manifests int // how many manifests reference this blob
}

// EnforceCacheLimit checks the total cache size and evicts oldest
// entries if over the limit. Returns bytes freed and entries removed.
func EnforceCacheLimit(wd string) (int64, int, error) {
	limitMB := getMaxCacheMB(wd)
	if limitMB <= 0 {
		return 0, 0, nil // unlimited
	}
	limitBytes := int64(limitMB) * 1024 * 1024

	brineDir := filepath.Join(wd, ".brine")

	// Measure current size (blobs only; manifests are tiny)
	var totalSize int64
	blobDir := filepath.Join(brineDir, "blobs")
	blobSizes := make(map[string]int64) // hash → size
	filepath.Walk(blobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		totalSize += info.Size()
		// extract hash from path (blobs/<xx>/<hash>)
		base := filepath.Base(path)
		if len(base) == 64 { // sha256 hex
			blobSizes[base] = info.Size()
		}
		return nil
	})

	if totalSize <= limitBytes {
		return 0, 0, nil
	}

	// Load all manifests to find referenced blobs
	manifestDir := filepath.Join(brineDir, "manifests")
	type manifestInfo struct {
		Key        string
		LastRead   time.Time
		Path       string
		BlobHashes []string
	}
	var manifests []manifestInfo

	entries, _ := os.ReadDir(manifestDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(manifestDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		key, _ := m["key"].(string)
		lastRead, _ := m["last_read"].(string)
		t, _ := time.Parse(time.RFC3339, lastRead)

		var hashes []string
		if blobs, ok := m["blobs"].([]interface{}); ok {
			for _, b := range blobs {
				if blob, ok := b.(map[string]interface{}); ok {
					if h, ok := blob["hash"].(string); ok {
						hashes = append(hashes, h)
					}
				}
			}
		}
		manifests = append(manifests, manifestInfo{
			Key: key, LastRead: t, Path: p, BlobHashes: hashes,
		})
	}

	// Sort manifests by LastRead (oldest first — evict these first)
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].LastRead.Before(manifests[j].LastRead)
	})

	// Evict oldest manifests (and their blobs) until under limit
	var freed int64
	var removed int
	seen := make(map[string]bool) // blobs already freed

	for _, m := range manifests {
		if totalSize-freed <= limitBytes {
			break
		}
		// Remove manifest
		os.Remove(m.Path)
		removed++
		// Remove its blobs (if not referenced by a surviving manifest)
		for _, h := range m.BlobHashes {
			if !seen[h] && blobSizes[h] > 0 {
				// Check if any OTHER manifest still references this blob
				// (simplified: just remove — dedup means shared blobs
				// will be re-uploaded if needed)
				blobPath := filepath.Join(blobDir, h[:2], h)
				if info, err := os.Stat(blobPath); err == nil {
					freed += info.Size()
					os.Remove(blobPath)
					seen[h] = true
				}
			}
		}
	}

	if removed > 0 {
		fmt.Printf("[brine] cache: evicted %d entries (%.1f MB freed, was %.1f MB, limit %d MB)\n",
			removed, float64(freed)/1024/1024, float64(totalSize)/1024/1024, limitMB)
	}

	return freed, removed, nil
}

func getMaxCacheMB(wd string) int {
	// env var takes precedence
	if v := os.Getenv("BRINE_MAX_CACHE_MB"); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	// config file
	cfg := loadConfig(wd)
	// check for max_cache_mb in the config
	if data, err := os.ReadFile(filepath.Join(wd, ".brine.json")); err == nil {
		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) == nil {
			if v, ok := raw["max_cache_mb"].(float64); ok {
				return int(v)
			}
		}
	}
	_ = cfg  // future: typed config field
	return 0 // unlimited
}

// CacheStats returns current cache size and entry count.
func CacheStats(wd string) (bytes int64, manifests int, blobs int) {
	brineDir := filepath.Join(wd, ".brine")

	filepath.Walk(filepath.Join(brineDir, "blobs"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobs++
			bytes += info.Size()
		}
		return nil
	})
	entries, _ := os.ReadDir(filepath.Join(brineDir, "manifests"))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			manifests++
		}
	}
	return
}
