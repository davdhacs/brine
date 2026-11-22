// Package metrics: per-run JSONL metrics for performance analysis
// and problem detection.
//
// Every brine invocation appends one JSON line to .brine/metrics.jsonl.
// The `brine stats` command summarizes the log. When a remote store
// is configured, each run also uploads its metrics blob for CI
// aggregation.
//
// Metrics answer:
//   - How fast was this run? (wall time, broken down by phase)
//   - Did it hit? (receipt hit/miss, strategy used)
//   - What did the cache do? (blobs read/written, bytes, hit rate)
//   - What did the protocol cache do? (GOCACHEPROG gets/hits/misses)
//   - Were there side-cache operations? (vendor/node_modules)
//   - Did anything go wrong? (errors, divergences, audit findings)
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Run is the metrics record for a single brine invocation.
type Run struct {
	// Identity
	Timestamp time.Time `json:"ts"`
	Command   string    `json:"command"`
	Key       string    `json:"key,omitempty"`
	Strategy  string    `json:"strategy,omitempty"`
	CWD       string    `json:"cwd,omitempty"`

	// Result
	Hit    bool  `json:"hit"`
	WallMs int64 `json:"wall_ms"` // total invocation time

	// Phase timing (ms)
	ClosureMs  int64 `json:"closure_ms,omitempty"`  // input discovery
	KeyMs      int64 `json:"key_ms,omitempty"`      // key computation
	BuildMs    int64 `json:"build_ms,omitempty"`    // command execution
	RestoreMs  int64 `json:"restore_ms,omitempty"`  // blob restore
	SnapshotMs int64 `json:"snapshot_ms,omitempty"` // output snapshot

	// Inputs/outputs
	InputCount  int64 `json:"input_count,omitempty"`
	OutputCount int64 `json:"output_count,omitempty"`
	OutputBytes int64 `json:"output_bytes,omitempty"`

	// Blob store operations
	BlobReads   int64 `json:"blob_reads,omitempty"`
	BlobWrites  int64 `json:"blob_writes,omitempty"`
	BlobBytesRd int64 `json:"blob_bytes_rd,omitempty"`
	BlobBytesWr int64 `json:"blob_bytes_wr,omitempty"`

	// GOCACHEPROG (go compile cache)
	GocacheGets     int64 `json:"gocache_gets,omitempty"`
	GocacheHits     int64 `json:"gocache_hits,omitempty"`
	GocacheMisses   int64 `json:"gocache_misses,omitempty"`
	GocachePuts     int64 `json:"gocache_puts,omitempty"`
	GocachePutBytes int64 `json:"gocache_put_bytes,omitempty"`

	// Protocol caches (ccache/sccache)
	ProtocolCache string `json:"protocol_cache,omitempty"` // "ccache" | "sccache"
	CcacheHits    int64  `json:"ccache_hits,omitempty"`
	CcacheMisses  int64  `json:"ccache_misses,omitempty"`

	// Side caches (vendor/node_modules)
	SideCacheRestored []string `json:"side_cache_restored,omitempty"`
	SideCacheSaved    []string `json:"side_cache_saved,omitempty"`

	// Safety events
	Divergences   int      `json:"divergences,omitempty"`
	AuditFindings []string `json:"audit_findings,omitempty"`

	// Errors
	Error string `json:"error,omitempty"`

	// Cache state (post-run)
	CacheBytes     int64 `json:"cache_bytes,omitempty"`
	CacheManifests int   `json:"cache_manifests,omitempty"`
}

// Logger appends metrics to a JSONL file.
type Logger struct {
	path string
}

// NewLogger creates a metrics logger rooted at the brine store dir.
func NewLogger(brineDir string) *Logger {
	return &Logger{
		path: filepath.Join(brineDir, "metrics.jsonl"),
	}
}

// Log appends a Run record to the JSONL file.
func (l *Logger) Log(r *Run) error {
	if l == nil || l.path == "" {
		return nil
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil // metrics should never break the build
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// LoadAll reads all metrics from the JSONL file.
func LoadAll(brineDir string) ([]Run, error) {
	path := filepath.Join(brineDir, "metrics.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		var r Run
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			runs = append(runs, r)
		}
	}
	return runs, nil
}

// Summarize produces aggregate statistics over a set of runs.
type Summary struct {
	TotalRuns   int     `json:"total_runs"`
	TotalHits   int     `json:"total_hits"`
	TotalMisses int     `json:"total_misses"`
	HitRate     float64 `json:"hit_rate"`

	AvgWallMs    int64 `json:"avg_wall_ms"`
	AvgHitMs     int64 `json:"avg_hit_ms"`
	AvgMissMs    int64 `json:"avg_miss_ms"`
	TotalWallMs  int64 `json:"total_wall_ms"`
	TotalSavedMs int64 `json:"total_saved_ms"` // estimated time saved vs cold

	TotalGocacheHits   int64 `json:"total_gocache_hits"`
	TotalGocacheMisses int64 `json:"total_gocache_misses"`
	TotalBlobReads     int64 `json:"total_blob_reads"`
	TotalBlobWrites    int64 `json:"total_blob_writes"`
	TotalDivergences   int   `json:"total_divergences"`

	StrategyCounts map[string]int `json:"strategy_counts"`
	Transfer       TransferStats  `json:"-"`
}

// Summarize computes aggregate stats.
func Summarize(runs []Run) *Summary {
	s := &Summary{StrategyCounts: make(map[string]int)}
	if len(runs) == 0 {
		return s
	}

	var totalHitMs, totalMissMs int64
	for _, r := range runs {
		s.TotalRuns++
		s.TotalWallMs += r.WallMs
		s.TotalGocacheHits += r.GocacheHits
		s.TotalGocacheMisses += r.GocacheMisses
		s.TotalBlobReads += r.BlobReads
		s.TotalBlobWrites += r.BlobWrites
		s.TotalDivergences += r.Divergences
		if r.Strategy != "" {
			s.StrategyCounts[r.Strategy]++
		}

		if r.Hit {
			s.TotalHits++
			totalHitMs += r.WallMs
		} else {
			s.TotalMisses++
			totalMissMs += r.WallMs
			// estimate: a hit would have been ~500ms
			s.TotalSavedMs += r.WallMs - 500
		}
	}

	s.HitRate = float64(s.TotalHits) / float64(s.TotalRuns) * 100
	s.AvgWallMs = s.TotalWallMs / int64(s.TotalRuns)
	if s.TotalHits > 0 {
		s.AvgHitMs = totalHitMs / int64(s.TotalHits)
	}
	if s.TotalMisses > 0 {
		s.AvgMissMs = totalMissMs / int64(s.TotalMisses)
	}
	return s
}

// Format renders a summary for display.
func (s *Summary) Format() string {
	if s.TotalRuns == 0 {
		return "no runs recorded"
	}
	out := fmt.Sprintf("runs: %d (hit: %d, miss: %d — %.1f%% hit rate)\n",
		s.TotalRuns, s.TotalHits, s.TotalMisses, s.HitRate)
	out += fmt.Sprintf("avg wall: %dms (hit: %dms, miss: %dms)\n",
		s.AvgWallMs, s.AvgHitMs, s.AvgMissMs)
	out += fmt.Sprintf("total wall: %dms, est. saved: %dms (%.1f min)\n",
		s.TotalWallMs, s.TotalSavedMs, float64(s.TotalSavedMs)/60000)
	if s.TotalGocacheHits+s.TotalGocacheMisses > 0 {
		out += fmt.Sprintf("gocache: %d hits / %d misses (%.0f%%)\n",
			s.TotalGocacheHits, s.TotalGocacheMisses,
			float64(s.TotalGocacheHits)/float64(s.TotalGocacheHits+s.TotalGocacheMisses)*100)
	}
	if s.TotalDivergences > 0 {
		out += fmt.Sprintf("divergences: %d\n", s.TotalDivergences)
	}
	if len(s.StrategyCounts) > 0 {
		out += "strategies:\n"
		for k, v := range s.StrategyCounts {
			out += fmt.Sprintf("  %s: %d\n", k, v)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
