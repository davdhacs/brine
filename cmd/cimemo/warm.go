// Warm mode: replay git history through cimemo to pre-seed the cache.
//
//	cimemo warm --last 10 make              # replay last 10 commits
//	cimemo warm --since v1.2.0 make         # replay from a tag
//	cimemo warm --metrics out.json ...      # collect per-commit stats
//
// Per-commit metrics include the "fresh runner" simulation: how many
// bytes of cache a new CI runner would need to download to get the
// same result (receipt blobs on hit, or action-cache entries used
// during the build).
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

// CommitMetrics is the per-commit record for the warming experiment.
type CommitMetrics struct {
	Sha          string `json:"sha"`
	Date         string `json:"date"`
	FilesAdded   int    `json:"files_added"`
	FilesChanged int    `json:"files_changed"`
	FilesDeleted int    `json:"files_deleted"`

	// Build result
	Cold     bool   `json:"cold"` // true = built, false = receipt hit
	WallMs   int64  `json:"wall_ms"`
	Strategy string `json:"strategy"`
	Outputs  int    `json:"outputs"`

	// GOCACHEPROG stats (Go builds)
	Gets     int64 `json:"gets,omitempty"`
	Hits     int64 `json:"hits,omitempty"`
	Misses   int64 `json:"misses,omitempty"`
	Puts     int64 `json:"puts,omitempty"`
	PutBytes int64 `json:"put_bytes,omitempty"`

	// Fresh-runner simulation: bytes a new runner would download
	// - receipt hit: output blob sizes
	// - receipt miss: action-cache entries that hit during the build
	DownloadBytes int64 `json:"download_bytes"`

	// Cache state after this commit
	CacheBytes     int64 `json:"cache_bytes"`
	CacheManifests int   `json:"cache_manifests"`
	CacheBlobs     int   `json:"cache_blobs"`
}

func cmdWarm(args []string) {
	var lastN int
	var sinceRef string
	var shas []string
	var metricsFile string
	var cmdArgs []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--last" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &lastN)
			i++
		} else if a == "--since" && i+1 < len(args) {
			sinceRef = args[i+1]
			i++
		} else if a == "--shas" && i+1 < len(args) {
			shas = strings.Split(args[i+1], ",")
			i++
		} else if a == "--metrics" && i+1 < len(args) {
			metricsFile = args[i+1]
			i++
		} else {
			cmdArgs = append(cmdArgs, a)
		}
	}

	if len(cmdArgs) == 0 {
		fatal("warm: specify a build command")
	}

	wd, _ := os.Getwd()

	// resolve commits
	var commits []string
	switch {
	case lastN > 0:
		out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"log", "--format=%H", fmt.Sprintf("-%d", lastN)).Output()
		if err != nil {
			fatal("git log: %v", err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				commits = append(commits, l)
			}
		}
	case sinceRef != "":
		out, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"log", "--format=%H", "--since="+sinceRef, "HEAD").Output()
		if err != nil {
			fatal("git log: %v", err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				commits = append(commits, l)
			}
		}
	case len(shas) > 0:
		commits = shas
	default:
		fatal("warm: --last N, --since <ref>, or --shas a,b,c")
	}
	// oldest first
	for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
		commits[i], commits[j] = commits[j], commits[i]
	}

	fmt.Printf("[cimemo] warm: %d commits, command: %s\n", len(commits), strings.Join(cmdArgs, " "))

	// save HEAD
	origHead, _ := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
		"rev-parse", "HEAD").Output()
	origHeadStr := strings.TrimSpace(string(origHead))
	defer func() {
		if origHeadStr != "" {
			exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
				"checkout", "-q", origHeadStr).Run()
		}
	}()

	var allMetrics []CommitMetrics
	var totalColdMs, totalWarmMs int64
	var coldCount, warmCount int
	var totalDownloadBytes int64

	cimemoSelf, _ := os.Executable()

	for idx, sha := range commits {
		short := sha[:12]
		fmt.Printf("[%d/%d] %s: ", idx+1, len(commits), short)

		// checkout
		if err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"checkout", "-q", sha).Run(); err != nil {
			fmt.Printf("checkout failed\n")
			continue
		}

		// get commit date and diff stats
		m := CommitMetrics{Sha: sha}
		if date, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"log", "-1", "--format=%ci", sha).Output(); err == nil {
			m.Date = strings.TrimSpace(string(date))
		}
		if diff, err := exec.Command("git", "-C", wd, "-c", "safe.directory="+wd,
			"diff", "--numstat", sha+"^", sha).Output(); err == nil {
			for _, line := range strings.Split(string(diff), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					var add, del int
					fmt.Sscanf(fields[0], "%d", &add)
					fmt.Sscanf(fields[1], "%d", &del)
					if add > 0 || del > 0 {
						m.FilesChanged++
					}
				}
			}
		}

		// run through cimemo
		start := time.Now()
		cmd := exec.Command(cimemoSelf, cmdArgs...)
		cmd.Dir = wd
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		wall := time.Since(start).Milliseconds()
		m.WallMs = wall

		output := string(out)
		if strings.Contains(output, "CACHE HIT") {
			m.Cold = false
			warmCount++
			totalWarmMs += wall
			fmt.Printf("HIT %dms", wall)
		} else {
			m.Cold = true
			coldCount++
			totalColdMs += wall
			fmt.Printf("MISS→built %dms", wall)
			if err != nil {
				fmt.Printf(" (error: %v)", err)
			}
		}

		// parse cimemo output for stats
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "strategy=") {
				parts := strings.Split(line, "strategy=")
				if len(parts) > 1 {
					m.Strategy = strings.TrimSpace(strings.Split(parts[1], " (")[0])
				}
			}
			// GOCACHEPROG stats
			if strings.Contains(line, "gocache:") {
				// parse: [cimemo] gocache: N gets (H hit, M miss), P puts (B bytes)
				var gets, hits, misses, puts int64
				var putBytes int64
				if n, _ := fmt.Sscanf(line,
					"[cimemo] gocache: %d gets (%d hit, %d miss), %d puts (%d bytes",
					&gets, &hits, &misses, &puts, &putBytes); n >= 4 {
					m.Gets = gets
					m.Hits = hits
					m.Misses = misses
					m.Puts = puts
					m.PutBytes = putBytes
				}
			}
		}

		// fresh-runner download simulation:
		// - receipt hit: the output blobs are what gets downloaded
		// - build: the action-cache entries that HIT are what gets downloaded
		if !m.Cold {
			// receipt hit: find the manifest and sum blob sizes
			if manifests, err := filepath.Glob(filepath.Join(wd, ".cimemo", "manifests", "*.json")); err == nil {
				for _, mf := range manifests {
					data, _ := os.ReadFile(mf)
					var mm map[string]interface{}
					if json.Unmarshal(data, &mm) == nil {
						if cmd, _ := mm["command"].(string); cmd == strings.Join(cmdArgs, " ") {
							if blobs, ok := mm["blobs"].([]interface{}); ok {
								for _, b := range blobs {
									if blob, ok := b.(map[string]interface{}); ok {
										if size, ok := blob["size"].(float64); ok {
											m.DownloadBytes += int64(size)
										}
									}
								}
							}
							break
						}
					}
				}
			}
		} else {
			// build: the action-cache hits are the download
			// (each hit is a cached compilation the runner didn't have to redo)
			// approximate: hits × avg entry size (~180KB from our measurements)
			m.DownloadBytes = m.Hits * 180 * 1024 // rough estimate
		}
		totalDownloadBytes += m.DownloadBytes

		// enforce cache limit after each build
		EnforceCacheLimit(wd)

		// cache stats
		cacheBytes, cacheManifests, cacheBlobs := CacheStats(wd)
		m.CacheBytes = cacheBytes
		m.CacheManifests = cacheManifests
		m.CacheBlobs = cacheBlobs

		fmt.Printf(" %s %dms dl=%.0fMB cache=%.1fGB\n",
			map[bool]string{false: "HIT", true: "BUILD"}[m.Cold],
			wall, float64(m.DownloadBytes)/1024/1024, float64(cacheBytes)/1024/1024/1024)
		allMetrics = append(allMetrics, m)
	}

	// summary
	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Printf("[cimemo] warm: %d commits\n", len(allMetrics))
	fmt.Printf("  cold: %d (%.0f%%) — %dms total\n",
		coldCount, float64(coldCount)/float64(len(allMetrics))*100, totalColdMs)
	fmt.Printf("  warm: %d (%.0f%%) — %dms total\n",
		warmCount, float64(warmCount)/float64(len(allMetrics))*100, totalWarmMs)
	if coldCount > 0 && warmCount > 0 {
		avgCold := totalColdMs / int64(coldCount)
		avgWarm := totalWarmMs / int64(warmCount)
		fmt.Printf("  avg cold: %dms, avg warm: %dms — %.1fx\n",
			avgCold, avgWarm, float64(avgCold)/float64(avgWarm))
	}
	fmt.Printf("  total fresh-runner download: %.1f GB\n",
		float64(totalDownloadBytes)/1024/1024/1024)

	// write metrics file
	if metricsFile != "" {
		data, _ := json.MarshalIndent(allMetrics, "", "  ")
		os.WriteFile(metricsFile, data, 0o644)
		fmt.Printf("  metrics: %s\n", metricsFile)
	}
}
