// Mtime normalization for go builds: the fix we proved in the CA
// branch, now automatic — but gated behind explicit user consent.
//
// Go's package-index hash includes file MTIMES. Git clone resets
// mtimes on every file. In CI (fresh clone per job), this means the
// action cache misses on ~25% of entries purely from the clock, not
// from content changes. Normalizing mtimes to a fixed epoch makes
// the hash content-only.
//
// SAFETY: normalizing SOURCE mtimes is safe for go builds (go uses
// mtimes for cache keys, not freshness decisions) but DANGEROUS for
// tools that use mtimes for dependency checking (make would skip
// needed rebuilds if all files have the same timestamp). This is why
// it's scoped to go builds AND requires user consent.
//
// Consent is given by EITHER:
//
//	.brine.json: {"normalize_mtimes": true}
//	env var:       BRINE_NORMALIZE_MTIMES=1
//
// On first run without consent, brine warns and skips normalization
// (the build still works; the action cache is just less effective).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// normalizeMtimesConsented reports whether the user has opted in.
func normalizeMtimesConsented(wd string) bool {
	if os.Getenv("BRINE_NORMALIZE_MTIMES") == "1" {
		return true
	}
	// check config file
	if data, err := os.ReadFile(filepath.Join(wd, ".brine.json")); err == nil {
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err == nil {
			if v, ok := raw["normalize_mtimes"].(bool); ok && v {
				return true
			}
		}
	}
	return false
}

// normalizeMtimes sets all file mtimes under the given roots to a
// fixed epoch. Returns the number of files touched.
func normalizeMtimes(roots ...string) int {
	const epoch = 946684800 // 2000-01-01
	t := time.Unix(epoch, 0)
	var total int

	for _, root := range roots {
		if root == "" {
			continue
		}
		if _, err := os.Stat(root); err != nil {
			continue
		}
		var count int
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			if err := os.Chtimes(path, t, t); err == nil {
				count++
			}
			return nil
		})
		total += count
	}
	return total
}

// prepareGoBuild applies go-specific optimizations before the build.
// Warns (once) if mtime normalization is not consented.
func prepareGoBuild(wd string) {
	// 1. Disable VCS stamping unless the user explicitly wants it.
	cfg := loadConfig(wd)
	if cfg.BuildVCS != "key-fold" {
		if !strings.Contains(os.Getenv("GOFLAGS"), "-buildvcs=false") {
			os.Setenv("GOFLAGS", strings.TrimSpace(os.Getenv("GOFLAGS")+" -buildvcs=false"))
		}
	}

	// 2. Mtime normalization — requires consent.
	if normalizeMtimesConsented(wd) {
		count := normalizeMtimes(wd)
		if gomodcache := goEnv("GOMODCACHE"); gomodcache != "" {
			count += normalizeMtimes(gomodcache)
		}
		if goroot := goEnv("GOROOT"); goroot != "" {
			count += normalizeMtimes(filepath.Join(goroot, "src"))
		}
		if count > 0 {
			fmt.Printf("[brine] mtimes: normalized %d files (content-only action cache keys)\n", count)
		}
	} else {
		// Warn once — this is a significant performance miss in CI.
		warnFile := filepath.Join(wd, ".brine", ".mtime-warning-shown")
		if _, err := os.Stat(warnFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, `
[brine] WARNING: mtime normalization is not enabled.
  Go's action cache includes file mtimes in its hash. Git checkout
  resets mtimes on every file. In CI (fresh clone per job), this
  causes ~25%% of cache entries to miss on every commit — even when
  the file content is identical.

  To enable (safe for go builds — go uses mtimes for cache keys,
  not freshness decisions):
    .brine.json: {"normalize_mtimes": true}
    or:           BRINE_NORMALIZE_MTIMES=1

  WARNING: do NOT enable if you run make or other mtime-dependent
  tools concurrently in the same directory.

`)
			os.MkdirAll(filepath.Dir(warnFile), 0o755)
			os.WriteFile(warnFile, []byte("shown"), 0o644)
		}
	}
}

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
